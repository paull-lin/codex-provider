package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type responseItem struct {
	Type             string `json:"type"`
	ID               string `json:"id"`
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	EncryptedContent string `json:"encrypted_content"`
	Content          []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
}
type responseData struct {
	ID     string         `json:"id"`
	Output []responseItem `json:"output"`
	Usage  struct {
		Input   int `json:"input_tokens"`
		Output  int `json:"output_tokens"`
		Details struct {
			Cached int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	Incomplete struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}
type responseEvent struct {
	Type        string        `json:"type"`
	OutputIndex int           `json:"output_index"`
	Delta       string        `json:"delta"`
	Item        responseItem  `json:"item"`
	Response    *responseData `json:"response"`
	raw         []byte
	err         error
}

// One bounded event is read ahead. Cancellation unblocks the producer even when
// the consumer exits at a terminal event; closing the HTTP body releases reads.
func readEvents(ctx context.Context, reader io.Reader) <-chan responseEvent {
	out := make(chan responseEvent)
	go func() {
		defer close(out)
		send := func(e responseEvent) bool {
			select {
			case out <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), 16<<20)
		var event string
		var data strings.Builder
		dispatch := func() bool {
			if data.Len() == 0 {
				return true
			}
			raw := strings.TrimSuffix(data.String(), "\n")
			data.Reset()
			if raw == "[DONE]" {
				return true
			}
			e := responseEvent{raw: []byte(raw)}
			if json.Unmarshal(e.raw, &e) != nil {
				e.err = fmt.Errorf("invalid upstream event")
			}
			if e.Type == "" {
				e.Type = event
			}
			return send(e)
		}
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if !dispatch() {
					return
				}
				event = ""
				continue
			}
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				event = value
			case "data":
				if data.Len()+len(value) > 16<<20 {
					send(responseEvent{err: fmt.Errorf("upstream event too large")})
					return
				}
				data.WriteString(value)
				data.WriteByte('\n')
			}
		}
		if !dispatch() {
			return
		}
		if scanner.Err() != nil {
			send(responseEvent{err: fmt.Errorf("cannot read upstream stream")})
		}
	}()
	return out
}

type outputBlock struct {
	item                                responseItem
	index                               int
	started, closed, textSeen, argsSeen bool
}
type converter struct {
	id, model        string
	started, hadTool bool
	blocks           map[int]*outputBlock
	order            []*outputBlock
	next             int
	emit             func(string, map[string]any) error
}

func newConverter(model string, emit func(string, map[string]any) error) *converter {
	return &converter{id: "msg_" + rand.Text(), model: model, blocks: map[int]*outputBlock{}, emit: emit}
}
func (c *converter) send(kind string, event map[string]any) error {
	event["type"] = kind
	return c.emit(kind, event)
}
func (c *converter) start() error {
	if c.started {
		return nil
	}
	c.started = true
	return c.send("message_start", map[string]any{"message": map[string]any{"id": c.id, "type": "message", "role": "assistant", "model": c.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "cache_read_input_tokens": 0, "output_tokens": 0}}})
}
func (c *converter) block(index int, item responseItem) *outputBlock {
	b := c.blocks[index]
	if b == nil {
		b = &outputBlock{index: -1}
		c.blocks[index] = b
		c.order = append(c.order, b)
	}
	// Keep metadata only. Text/arguments/encrypted payloads are forwarded as
	// deltas, never accumulated in streaming mode (even across large tool calls).
	if item.Type != "" {
		b.item.Type = item.Type
	}
	if item.ID != "" {
		b.item.ID = item.ID
	}
	if item.CallID != "" {
		b.item.CallID = item.CallID
	}
	if item.Name != "" {
		b.item.Name = item.Name
	}
	return b
}
func (c *converter) startBlock(b *outputBlock) error {
	if b.started {
		return nil
	}
	if err := c.start(); err != nil {
		return err
	}
	var content map[string]any
	switch b.item.Type {
	case "message":
		content = map[string]any{"type": "text", "text": ""}
	case "reasoning":
		content = map[string]any{"type": "thinking", "thinking": ""}
	case "function_call":
		if b.item.CallID == "" || b.item.Name == "" {
			return fmt.Errorf("upstream tool call missing identity")
		}
		content = map[string]any{"type": "tool_use", "id": b.item.CallID, "name": b.item.Name, "input": map[string]any{}}
		c.hadTool = true
	default:
		return fmt.Errorf("unsupported upstream output item")
	}
	b.started = true
	b.index = c.next
	c.next++
	return c.send("content_block_start", map[string]any{"index": b.index, "content_block": content})
}
func (c *converter) delta(b *outputBlock, kind, field, value string) error {
	if b.closed {
		return fmt.Errorf("upstream delta after block end")
	}
	if err := c.startBlock(b); err != nil {
		return err
	}
	return c.send("content_block_delta", map[string]any{"index": b.index, "delta": map[string]any{"type": kind, field: value}})
}
func (c *converter) closeBlock(b *outputBlock) error {
	if !b.started || b.closed {
		return nil
	}
	b.closed = true
	return c.send("content_block_stop", map[string]any{"index": b.index})
}
func (c *converter) done(index int, item responseItem) error {
	b := c.block(index, item)
	if b.closed {
		return nil
	}
	switch b.item.Type {
	case "message":
		if !b.textSeen {
			for _, p := range item.Content {
				if p.Type == "output_text" {
					if err := c.delta(b, "text_delta", "text", p.Text); err != nil {
						return err
					}
				}
			}
		}
	case "reasoning":
		if !b.textSeen {
			for _, p := range item.Summary {
				if err := c.delta(b, "thinking_delta", "thinking", p.Text); err != nil {
					return err
				}
			}
		}
		// Even a reasoning item with no public summary must survive a tool turn.
		// An empty thinking block carries its opaque encrypted replay signature.
		if item.EncryptedContent != "" {
			raw, _ := json.Marshal(map[string]string{"id": b.item.ID, "encrypted_content": item.EncryptedContent})
			if err := c.delta(b, "signature_delta", "signature", "cxr1:"+base64.StdEncoding.EncodeToString(raw)); err != nil {
				return err
			}
		}
	case "function_call":
		if err := c.startBlock(b); err != nil {
			return err
		}
		if !b.argsSeen {
			args := item.Arguments
			if args == "" {
				args = "{}"
			}
			if err := c.delta(b, "input_json_delta", "partial_json", args); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported upstream output item")
	}
	return c.closeBlock(b)
}
func (c *converter) step(e responseEvent) (bool, error) {
	if e.Response != nil && e.Response.ID != "" && !c.started {
		c.id = e.Response.ID
	}
	switch e.Type {
	case "response.output_item.added":
		b := c.block(e.OutputIndex, e.Item)
		if e.Item.Type == "function_call" {
			return false, c.startBlock(b)
		}
	case "response.output_text.delta":
		b := c.block(e.OutputIndex, responseItem{Type: "message"})
		b.textSeen = true
		return false, c.delta(b, "text_delta", "text", e.Delta)
	case "response.reasoning_summary_text.delta":
		b := c.block(e.OutputIndex, responseItem{Type: "reasoning"})
		b.textSeen = true
		return false, c.delta(b, "thinking_delta", "thinking", e.Delta)
	case "response.function_call_arguments.delta":
		b := c.block(e.OutputIndex, responseItem{Type: "function_call"})
		b.argsSeen = true
		return false, c.delta(b, "input_json_delta", "partial_json", e.Delta)
	case "response.output_item.done":
		return false, c.done(e.OutputIndex, e.Item)
	case "response.completed", "response.incomplete":
		if e.Response == nil {
			return false, fmt.Errorf("upstream completed without response")
		}
		for i, item := range e.Response.Output {
			if err := c.done(i, item); err != nil {
				return false, err
			}
		}
		for _, b := range c.order {
			if err := c.closeBlock(b); err != nil {
				return false, err
			}
		}
		if err := c.start(); err != nil {
			return false, err
		}
		reason := "end_turn"
		if c.hadTool {
			reason = "tool_use"
		}
		if e.Type == "response.incomplete" && e.Response.Incomplete.Reason == "max_output_tokens" {
			reason = "max_tokens"
		}
		u := e.Response.Usage
		cached := min(max(0, u.Details.Cached), max(0, u.Input))
		usage := map[string]int{"input_tokens": max(0, u.Input-cached), "cache_read_input_tokens": cached, "output_tokens": max(0, u.Output)}
		if err := c.send("message_delta", map[string]any{"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil}, "usage": usage}); err != nil {
			return false, err
		}
		return true, c.send("message_stop", map[string]any{})
	}
	return false, nil
}

type collectedBlock struct {
	content               map[string]any
	text, args, signature strings.Builder
}
type collector struct {
	message map[string]any
	blocks  []*collectedBlock
}

// Non-streaming clients aggregate the very same emitted Anthropic events. There
// is no second Responses parser whose tool order/usage/signatures can diverge.
func (a *collector) emit(kind string, event map[string]any) error {
	switch kind {
	case "message_start":
		a.message = event["message"].(map[string]any)
	case "content_block_start":
		a.blocks = append(a.blocks, &collectedBlock{content: event["content_block"].(map[string]any)})
	case "content_block_delta":
		b := a.blocks[event["index"].(int)]
		d := event["delta"].(map[string]any)
		switch d["type"] {
		case "text_delta":
			b.text.WriteString(d["text"].(string))
		case "thinking_delta":
			b.text.WriteString(d["thinking"].(string))
		case "signature_delta":
			b.signature.WriteString(d["signature"].(string))
		case "input_json_delta":
			b.args.WriteString(d["partial_json"].(string))
		}
	case "content_block_stop":
		b := a.blocks[event["index"].(int)]
		switch b.content["type"] {
		case "text":
			b.content["text"] = b.text.String()
		case "thinking":
			b.content["thinking"] = b.text.String()
			b.content["signature"] = b.signature.String()
		case "tool_use":
			if b.args.Len() > 0 {
				var input map[string]any
				if json.Unmarshal([]byte(b.args.String()), &input) != nil || input == nil {
					return fmt.Errorf("invalid upstream tool arguments")
				}
				b.content["input"] = input
			}
		}
	case "message_delta":
		d := event["delta"].(map[string]any)
		a.message["stop_reason"] = d["stop_reason"]
		a.message["usage"] = event["usage"]
	case "message_stop":
		content := make([]any, 0, len(a.blocks))
		for _, b := range a.blocks {
			content = append(content, b.content)
		}
		a.message["content"] = content
	}
	return nil
}

func (a *adapter) relay(w http.ResponseWriter, r *http.Request, body io.Reader, model string, stream bool) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	collected := &collector{}
	committed := false
	emit := func(kind string, event map[string]any) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !stream {
			return collected.emit(kind, event)
		}
		if !committed {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			committed = true
		}
		raw, _ := json.Marshal(event)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, raw); err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	}
	fail := func(e *apiError) {
		if ctx.Err() != nil {
			return
		}
		if committed {
			_ = emit("error", map[string]any{"type": "error", "error": map[string]any{"type": e.kind, "message": e.message}})
		} else {
			writeAnthropicError(w, e.status, e.kind, e.message)
		}
	}
	c := newConverter(model, emit)
	interval := a.pingInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	events := readEvents(ctx, body)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Lifecycle events alone do not commit HTTP 200. After the heartbeat
			// deadline we intentionally commit; subsequent failures must be SSE errors.
			if stream {
				if c.start() != nil {
					return
				}
				if c.send("ping", map[string]any{}) != nil {
					return
				}
			}
		case event, ok := <-events:
			if !ok {
				fail(&apiError{502, "api_error", "upstream stream ended before completion"})
				return
			}
			if event.err != nil {
				fail(&apiError{502, "api_error", event.err.Error()})
				return
			}
			if event.Type == "error" || event.Type == "response.failed" {
				fail(upstreamError(0, event.raw))
				return
			}
			terminal, err := c.step(event)
			if err != nil {
				fail(&apiError{502, "api_error", "invalid or interrupted upstream stream"})
				return
			}
			if terminal {
				if !stream {
					writeJSON(w, 200, collected.message)
				}
				return
			}
		}
	}
}

type apiError struct {
	status        int
	kind, message string
}

func upstreamError(status int, raw []byte) *apiError {
	var root map[string]any
	_ = json.Unmarshal(raw, &root)
	if status == 0 {
		if s, ok := root["status"].(float64); ok {
			status = int(s)
		}
	}
	if response, ok := root["response"].(map[string]any); ok {
		root = response
	}
	if nested, ok := root["error"].(map[string]any); ok {
		root = nested
	}
	code, _ := root["code"].(string)
	typ, _ := root["type"].(string)
	message, _ := root["message"].(string)
	// The pool can relay native Codex codes without an HTTP status in SSE.
	switch strings.ToLower(code) {
	case "slow_down", "insufficient_quota", "usage_not_included":
		status = 529
	case "account_auth_failed", "invalid_token", "token_invalidated", "token_revoked", "unauthorized", "forbidden":
		status = 401
	case "context_length_exceeded", "invalid_prompt", "cyber_policy", "content_policy_violation", "policy_violation":
		status = 400
	}
	label := strings.ToLower(code + " " + typ + " " + message)
	// Match upstream classifications, but never echo a raw error body: providers
	// can include request/account data there. No keys or bodies enter logs either.
	switch {
	case status == 529 || status == 503 || strings.Contains(label, "server_is_overloaded") || strings.Contains(label, "capacity") || strings.Contains(label, "overloaded"):
		return &apiError{529, "overloaded_error", "upstream capacity is temporarily unavailable"}
	case status == 429 || strings.Contains(label, "rate_limit") || strings.Contains(label, "usage_limit"):
		return &apiError{429, "rate_limit_error", "upstream rate limit exceeded"}
	case status == 401 || strings.Contains(label, "authentication") || strings.Contains(label, "invalid_api_key"):
		return &apiError{401, "authentication_error", "upstream authentication failed"}
	case status == 400 || strings.Contains(label, "invalid_request"):
		return &apiError{400, "invalid_request_error", "upstream rejected the request"}
	default:
		return &apiError{502, "api_error", "upstream request failed"}
	}
}
