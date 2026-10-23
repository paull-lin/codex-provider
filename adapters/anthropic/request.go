package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type anthropicRequest struct {
	Model     string             `json:"model"`
	Messages  []anthropicMessage `json:"messages"`
	System    json.RawMessage    `json:"system"`
	Stream    bool               `json:"stream"`
	MaxTokens int                `json:"max_tokens"`
	Thinking  *struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	} `json:"thinking"`
	OutputConfig *struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
	Tools      []anthropicTool `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
	Metadata   map[string]any  `json:"metadata"`
}
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}
type anthropicTool struct {
	Type           string         `json:"type,omitempty"`
	Name           string         `json:"name"`
	Description    string         `json:"description"`
	InputSchema    map[string]any `json:"input_schema"`
	AllowedDomains []string       `json:"allowed_domains,omitempty"`
	UserLocation   map[string]any `json:"user_location,omitempty"`
}
type inputBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Signature string          `json:"signature"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

func decodeBlocks(raw json.RawMessage) ([]inputBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []inputBlock{{Type: "text", Text: text}}, nil
	}
	var blocks []inputBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, fmt.Errorf("content must be text or an array of blocks")
	}
	return blocks, nil
}

func requestEffort(r anthropicRequest) string {
	if r.OutputConfig != nil {
		switch r.OutputConfig.Effort {
		case "low", "medium", "high":
			return r.OutputConfig.Effort
		case "max", "xhigh":
			return "high"
		}
	}
	if r.Thinking == nil || r.Thinking.Type == "disabled" {
		return ""
	}
	switch {
	case r.Thinking.BudgetTokens >= 16000:
		return "high"
	case r.Thinking.BudgetTokens >= 8000:
		return "medium"
	case r.Thinking.BudgetTokens > 0:
		return "low"
	case r.Thinking.Type == "adaptive":
		return "medium"
	}
	return ""
}

// Preserve explicit conversation IDs. Hash only the initial prompt fallback so
// later turns keep pool affinity. Compaction/dynamic system prompts need an
// explicit ID; this stateless adapter cannot infer continuity after those change.
func sessionID(r *http.Request, req anthropicRequest) string {
	if id := strings.TrimSpace(r.Header.Get("X-Claude-Code-Session-Id")); id != "" {
		return id
	}
	if id, ok := req.Metadata["user_id"].(string); ok && strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	var first json.RawMessage
	for _, message := range req.Messages {
		if message.Role == "user" {
			first = message.Content
			break
		}
	}
	// Decode/re-encode JSON to avoid formatting-dependent identity and include
	// image-only first messages. A structured pair avoids concatenation collisions.
	var systemValue, firstValue any
	_ = json.Unmarshal(req.System, &systemValue)
	_ = json.Unmarshal(first, &firstValue)
	material, _ := json.Marshal([]any{systemValue, firstValue})
	sum := sha256.Sum256(material)
	return fmt.Sprintf("anthropic-%x", sum[:16])
}

func mapRequest(req anthropicRequest) ([]any, error) {
	input := make([]any, 0)
	if len(req.System) > 0 && string(req.System) != "null" {
		blocks, err := decodeBlocks(req.System)
		if err != nil {
			return nil, err
		}
		content := make([]any, 0, len(blocks))
		for _, b := range blocks {
			if b.Type != "text" {
				return nil, fmt.Errorf("system supports only text blocks")
			}
			content = append(content, map[string]any{"type": "input_text", "text": b.Text})
		}
		if len(content) > 0 {
			input = append(input, map[string]any{"type": "message", "role": "developer", "content": content})
		}
	}
	for _, message := range req.Messages {
		if message.Role == "system" {
			// Claude Code 2.x interleaves system messages (reminders, mode
			// changes) into messages[]. Keep each at its position as a developer
			// message so ordering and intent survive; rejecting them made every
			// real Claude Code request fail with 400 even though tests passed.
			blocks, err := decodeBlocks(message.Content)
			if err != nil {
				return nil, err
			}
			var content []any
			for _, b := range blocks {
				if b.Type == "text" && b.Text != "" {
					content = append(content, map[string]any{"type": "input_text", "text": b.Text})
				}
			}
			if len(content) > 0 {
				input = append(input, map[string]any{"type": "message", "role": "developer", "content": content})
			}
			continue
		}
		if message.Role != "user" && message.Role != "assistant" {
			return nil, fmt.Errorf("message role must be user or assistant")
		}
		blocks, err := decodeBlocks(message.Content)
		if err != nil {
			return nil, err
		}
		var content []any
		flush := func() {
			if len(content) > 0 {
				input = append(input, map[string]any{"type": "message", "role": message.Role, "content": content})
				content = nil
			}
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				typ := "input_text"
				if message.Role == "assistant" {
					typ = "output_text"
				}
				content = append(content, map[string]any{"type": typ, "text": b.Text})
			case "image":
				if message.Role != "user" {
					return nil, fmt.Errorf("images require a user message")
				}
				image, err := mapImage(b)
				if err != nil {
					return nil, err
				}
				content = append(content, image)
			case "tool_use":
				if message.Role != "assistant" || b.ID == "" || b.Name == "" {
					return nil, fmt.Errorf("tool_use requires assistant role, id and name")
				}
				var args map[string]any
				if json.Unmarshal(b.Input, &args) != nil || args == nil {
					return nil, fmt.Errorf("tool_use input must be an object")
				}
				encoded, _ := json.Marshal(args)
				flush()
				input = append(input, map[string]any{"type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": string(encoded)})
			case "tool_result":
				if message.Role != "user" || b.ToolUseID == "" {
					return nil, fmt.Errorf("tool_result requires user role and tool_use_id")
				}
				output, err := toolOutput(b)
				if err != nil {
					return nil, err
				}
				flush()
				input = append(input, map[string]any{"type": "function_call_output", "call_id": b.ToolUseID, "output": output})
			case "thinking":
				if message.Role == "assistant" {
					flush()
					if item := restoreReasoning(b.Signature); item != nil {
						input = append(input, item)
					}
				}
			case "redacted_thinking":
				// Foreign reasoning is not recoverable on the Responses backend.
			case "server_tool_use", "web_search_tool_result":
				// Hosted search has already run upstream. Replay the assistant's
				// summary text, not synthetic function calls or opaque search results.
				if message.Role != "assistant" {
					return nil, fmt.Errorf("server tool history requires assistant role")
				}
			default:
				return nil, fmt.Errorf("unsupported message content block")
			}
		}
		flush()
	}
	return input, nil
}

func mapImage(b inputBlock) (map[string]any, error) {
	if b.Source == nil || b.Source.Type != "base64" || b.Source.Data == "" {
		return nil, fmt.Errorf("images require a base64 source")
	}
	switch b.Source.MediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return nil, fmt.Errorf("unsupported image media_type")
	}
	if _, err := base64.StdEncoding.DecodeString(b.Source.Data); err != nil {
		return nil, fmt.Errorf("invalid base64 image")
	}
	return map[string]any{"type": "input_image", "image_url": "data:" + b.Source.MediaType + ";base64," + b.Source.Data}, nil
}

func toolOutput(b inputBlock) (any, error) {
	blocks, err := decodeBlocks(b.Content)
	if err != nil {
		return nil, err
	}
	parts := make([]any, 0, len(blocks))
	texts := make([]string, 0, len(blocks))
	hasImage := false
	// Responses has no is_error flag. Keep it visible in the tool output instead
	// of silently turning a failed Claude tool invocation into a success.
	if b.IsError {
		parts = append(parts, map[string]any{"type": "input_text", "text": "[tool_result is_error=true]"})
		texts = append(texts, "[tool_result is_error=true]")
	}
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "input_text", "text": block.Text})
			texts = append(texts, block.Text)
		case "image":
			image, err := mapImage(block)
			if err != nil {
				return nil, err
			}
			parts = append(parts, image)
			hasImage = true
		default:
			return nil, fmt.Errorf("unsupported tool_result block")
		}
	}
	if hasImage {
		return parts, nil
	}
	return strings.Join(texts, "\n"), nil
}

// cxr1 is a transport envelope, not an authentication signature. Restore only
// its id/encrypted_content; never promote arbitrary fields or plain thinking.
func restoreReasoning(signature string) map[string]any {
	if !strings.HasPrefix(signature, "cxr1:") {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(signature, "cxr1:"))
	if err != nil {
		return nil
	}
	var item struct {
		ID               string `json:"id"`
		EncryptedContent string `json:"encrypted_content"`
	}
	if json.Unmarshal(raw, &item) != nil || item.EncryptedContent == "" {
		return nil
	}
	// The Responses API requires `summary` on every replayed reasoning item; the
	// fake-pool tests accepted it without, but real upstream rejected each tool
	// turn with "Missing required parameter: input[N].summary". The summary
	// text is not needed for continuity (encrypted_content carries it).
	out := map[string]any{"type": "reasoning", "encrypted_content": item.EncryptedContent, "summary": []any{}}
	if item.ID != "" {
		out["id"] = item.ID
	}
	return out
}

func mapTools(tools []anthropicTool) ([]any, error) {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		if strings.HasPrefix(t.Type, "web_search_") {
			// Native CLI 0.163 capture (WIRE.md); Codex rust-v0.163.0
			// core/tests/suite/web_search.rs verifies live access, filters/location.
			// Keep include unchanged: sources are optional, citations are fallback.
			tool := map[string]any{"type": "web_search", "external_web_access": true, "search_content_types": []string{"text", "image"}}
			if t.AllowedDomains != nil {
				tool["filters"] = map[string]any{"allowed_domains": t.AllowedDomains}
			}
			if t.UserLocation != nil {
				tool["user_location"] = t.UserLocation
			}
			// max_uses and blocked_domains have no mapping in this native shape.
			out = append(out, tool)
			continue
		}
		if (t.Type != "" && t.Type != "custom") || t.Name == "" || t.InputSchema == nil {
			return nil, fmt.Errorf("only custom tools with name and input_schema are supported")
		}
		out = append(out, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.InputSchema, "strict": false})
	}
	return out, nil
}
func mapToolChoice(raw json.RawMessage, tools []anthropicTool) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &choice) != nil {
		if json.Unmarshal(raw, &choice.Type) != nil {
			return nil, fmt.Errorf("invalid tool_choice")
		}
	}
	switch choice.Type {
	case "auto", "none":
		return choice.Type, nil
	case "any":
		return "required", nil
	case "tool":
		if choice.Name != "" {
			for _, tool := range tools {
				if tool.Name == choice.Name && strings.HasPrefix(tool.Type, "web_search_") {
					// Hosted searches are not client-executed named functions.
					return "auto", nil
				}
			}
			return map[string]any{"type": "function", "name": choice.Name}, nil
		}
	}
	return nil, fmt.Errorf("invalid tool_choice")
}
