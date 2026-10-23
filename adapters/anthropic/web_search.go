package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Codex rust-v0.163.0 codex-rs/protocol/src/models.rs, WebSearchCall and
// WebSearchAction: action/query/queries are optional, and actions can also be
// open_page/find_in_page or unknown. Do not require a search query to decode.
// Sources are an optional backend extension; native include does not request them.
type webSearchAction struct {
	Query   *string           `json:"query"`
	Queries []string          `json:"queries"`
	Sources []webSearchSource `json:"sources"`
}

type webSearchSource struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

func webSearchID(id string, fallback int) string {
	if id == "" {
		id = fmt.Sprintf("search_%d", fallback)
	}
	return "srvtoolu_" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, id)
}

func (c *converter) addCitation(source webSearchSource) {
	if source.Type != "url_citation" || source.URL == "" {
		return
	}
	for _, existing := range c.citations {
		if existing.URL == source.URL {
			return
		}
	}
	c.citations = append(c.citations, source)
}

func (c *converter) searchUse(b *outputBlock) error {
	input := map[string]any{}
	if action := b.item.Action; action != nil {
		if action.Query != nil {
			input["query"] = *action.Query
		} else if len(action.Queries) > 0 {
			input["query"] = action.Queries[0]
		}
	}
	raw, _ := json.Marshal(input)
	if err := c.delta(b, "input_json_delta", "partial_json", string(raw)); err != nil {
		return err
	}
	return c.closeBlock(b)
}

func (c *converter) searchResults() error {
	// Citations can arrive after streamed text, even only in response.completed.
	// Defer results until all items close, then allocate fresh sequential indices
	// before message_delta. Never reopen an old text block or buffer its deltas.
	// Anthropic web-search-tool streaming sends result content in block_start,
	// while server_tool_use follows the tool_use input_json_delta protocol.
	for _, b := range c.order {
		if b.item.Type != "web_search_call" {
			continue
		}
		sources := c.citations
		if b.item.Action != nil && b.item.Action.Sources != nil {
			sources = b.item.Action.Sources
		}
		results := make([]any, 0, len(sources))
		seen := make(map[string]bool)
		for _, source := range sources {
			if source.URL == "" || seen[source.URL] {
				continue
			}
			seen[source.URL] = true
			results = append(results, map[string]any{"type": "web_search_result", "url": source.URL, "title": source.Title, "encrypted_content": "", "page_age": nil})
		}
		var content any = results
		if b.item.Status == "failed" {
			content = map[string]any{"type": "web_search_tool_result_error", "error_code": "unavailable"}
		}
		index := c.next
		c.next++
		block := map[string]any{"type": "web_search_tool_result", "tool_use_id": b.searchID, "content": content}
		if err := c.send("content_block_start", map[string]any{"index": index, "content_block": block}); err != nil {
			return err
		}
		if err := c.send("content_block_stop", map[string]any{"index": index}); err != nil {
			return err
		}
	}
	return nil
}
