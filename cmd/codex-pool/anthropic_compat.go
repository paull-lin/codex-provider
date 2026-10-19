package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
)

// Anthropic's Messages API is the wire contract used by Claude Code when
// ANTHROPIC_BASE_URL points at this service. Keep the translation at the HTTP
// boundary and delegate routing, account failover, quota, and upstream auth to
// the existing OpenAI-compatible proxy path.
func (a *app) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil || len(body) > maxRequestBody {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body is invalid or too large")
		return
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body must be JSON")
		return
	}
	model, _ := request["model"].(string)
	if strings.TrimSpace(model) == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if _, ok := request["messages"].([]any); !ok {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages must be an array")
		return
	}
	chatRequest, err := anthropicToChatRequest(request)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	chatBody, err := json.Marshal(chatRequest)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "unable to encode request")
		return
	}

	// Calling the existing handler through a recorder keeps this adapter from
	// duplicating sticky routing and failover behavior. Streaming is translated
	// after the upstream response is buffered; this preserves the Anthropic SSE
	// contract while keeping one account-selection implementation.
	proxyRequest := r.Clone(r.Context())
	proxyRequest.Method = http.MethodPost
	urlCopy := *r.URL
	proxyRequest.URL = &urlCopy
	proxyRequest.URL.Path = "/v1/chat/completions"
	proxyRequest.Body = io.NopCloser(bytes.NewReader(chatBody))
	recorder := httptest.NewRecorder()
	a.handleProxy(recorder, proxyRequest, true)

	if recorder.Code < 200 || recorder.Code >= 300 {
		writeAnthropicProxyError(w, recorder.Code, recorder.Body.Bytes())
		return
	}
	if streaming, _ := request["stream"].(bool); streaming {
		writeAnthropicStream(w, recorder.Body.Bytes(), model)
		return
	}
	message, err := chatToAnthropicResponse(recorder.Body.Bytes(), model)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "invalid OpenAI-compatible response from upstream")
		return
	}
	for key, values := range recorder.Header() {
		if strings.HasPrefix(strings.ToLower(key), "x-codex-pool-") {
			w.Header()[key] = append([]string(nil), values...)
		}
	}
	writeJSON(w, http.StatusOK, message)
}

func (a *app) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil || len(body) > maxRequestBody {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body is invalid or too large")
		return
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body must be JSON")
		return
	}
	chatRequest, err := anthropicToChatRequest(request)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	encoded, _ := json.Marshal(chatRequest)
	// The gateway does not expose Anthropic's tokenizer. A deterministic
	// character estimate is preferable to omitting the endpoint: Claude Code
	// uses this value for budgeting, while the actual upstream usage remains the
	// source of truth for billing and cache telemetry.
	tokens := (len(encoded) + 3) / 4
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokens})
}

func anthropicToChatRequest(request map[string]any) (map[string]any, error) {
	messages := make([]map[string]any, 0)
	if system, ok := request["system"]; ok {
		content, err := anthropicContentText(system)
		if err != nil {
			return nil, fmt.Errorf("system: %w", err)
		}
		if content != "" {
			messages = append(messages, map[string]any{"role": "system", "content": content})
		}
	}
	rawMessages, _ := request["messages"].([]any)
	for _, raw := range rawMessages {
		message, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("message must be an object")
		}
		role, _ := message["role"].(string)
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("message role must be user or assistant")
		}
		blocks, err := anthropicContentBlocks(message["content"])
		if err != nil {
			return nil, err
		}
		if role == "assistant" {
			assistant := map[string]any{"role": role, "content": ""}
			toolCalls := make([]map[string]any, 0)
			for _, block := range blocks {
				switch block["type"] {
				case "text":
					assistant["content"] = assistant["content"].(string) + block["text"].(string)
				case "tool_use":
					arguments, _ := json.Marshal(block["input"])
					toolCalls = append(toolCalls, map[string]any{"id": block["id"], "type": "function", "function": map[string]any{"name": block["name"], "arguments": string(arguments)}})
				}
			}
			if len(toolCalls) > 0 {
				assistant["tool_calls"] = toolCalls
			}
			messages = append(messages, assistant)
			continue
		}
		text := ""
		for _, block := range blocks {
			switch block["type"] {
			case "text":
				text += block["text"].(string)
			case "tool_result":
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": block["tool_use_id"], "content": block["content"]})
			}
		}
		if text != "" || len(blocks) == 0 {
			messages = append(messages, map[string]any{"role": role, "content": text})
		}
	}
	result := map[string]any{"model": request["model"], "messages": messages}
	for _, name := range []string{"max_tokens", "temperature", "top_p", "stream", "stop_sequences", "metadata"} {
		if value, ok := request[name]; ok {
			result[name] = value
		}
	}
	if rawTools, ok := request["tools"].([]any); ok {
		tools := make([]map[string]any, 0, len(rawTools))
		for _, raw := range rawTools {
			tool, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("tool must be an object")
			}
			tools = append(tools, map[string]any{
				"type":     "function",
				"function": map[string]any{"name": tool["name"], "description": tool["description"], "parameters": tool["input_schema"]},
			})
		}
		result["tools"] = tools
	}
	if choice, ok := request["tool_choice"].(map[string]any); ok {
		choiceType, _ := choice["type"].(string)
		switch choiceType {
		case "auto":
			result["tool_choice"] = "auto"
		case "any":
			result["tool_choice"] = "required"
		case "tool":
			result["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": choice["name"]}}
		}
	}
	return result, nil
}

func anthropicContentText(value any) (string, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	blocks, ok := value.([]any)
	if !ok {
		return "", fmt.Errorf("content must be a string or block array")
	}
	var builder strings.Builder
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("content block must be an object")
		}
		switch blockType, _ := block["type"].(string); blockType {
		case "text":
			if text, ok := block["text"].(string); ok {
				builder.WriteString(text)
			}
		case "tool_result":
			if content, ok := block["content"]; ok {
				text, err := anthropicContentText(content)
				if err != nil {
					return "", err
				}
				builder.WriteString(text)
			}
		case "image":
			return "", fmt.Errorf("image content is not supported by this compatibility layer")
		case "thinking", "redacted_thinking":
			// Thinking blocks are provider-specific and must not be sent as user
			// text. Preserve the surrounding conversation without exposing them.
		default:
			return "", fmt.Errorf("unsupported content block type %q", blockType)
		}
	}
	return builder.String(), nil
}

func anthropicContentBlocks(value any) ([]map[string]any, error) {
	if text, ok := value.(string); ok {
		return []map[string]any{{"type": "text", "text": text}}, nil
	}
	rawBlocks, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("content must be a string or block array")
	}
	blocks := make([]map[string]any, 0, len(rawBlocks))
	for _, raw := range rawBlocks {
		block, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("content block must be an object")
		}
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			if _, ok := block["text"].(string); !ok {
				return nil, fmt.Errorf("text content block must contain text")
			}
			blocks = append(blocks, block)
		case "tool_use":
			if _, ok := block["id"].(string); !ok {
				return nil, fmt.Errorf("tool_use block must contain id")
			}
			blocks = append(blocks, block)
		case "tool_result":
			content, err := anthropicContentText(block["content"])
			if err != nil {
				return nil, err
			}
			block["content"] = content
			blocks = append(blocks, block)
		case "thinking", "redacted_thinking":
			// Provider-specific thinking is not representable in Chat Completions.
		default:
			return nil, fmt.Errorf("unsupported content block type %q", blockType)
		}
	}
	return blocks, nil
}

func chatToAnthropicResponse(body []byte, model string) (map[string]any, error) {
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	choices, _ := response["choices"].([]any)
	if len(choices) == 0 {
		return nil, fmt.Errorf("missing choices")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	content := make([]map[string]any, 0, 2)
	if text, ok := message["content"].(string); ok && text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			function, _ := call["function"].(map[string]any)
			input := map[string]any{}
			if arguments, ok := function["arguments"].(string); ok && arguments != "" {
				_ = json.Unmarshal([]byte(arguments), &input)
			}
			content = append(content, map[string]any{"type": "tool_use", "id": call["id"], "name": function["name"], "input": input})
		}
	}
	stopReason := "end_turn"
	if finish, _ := choice["finish_reason"].(string); finish == "tool_calls" {
		stopReason = "tool_use"
	} else if finish == "length" {
		stopReason = "max_tokens"
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if rawUsage, ok := response["usage"].(map[string]any); ok {
		usage["input_tokens"] = rawUsage["prompt_tokens"]
		usage["output_tokens"] = rawUsage["completion_tokens"]
	}
	id, _ := response["id"].(string)
	if id == "" {
		id = "msg_" + randomID()
	}
	return map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": content, "stop_reason": stopReason, "stop_sequence": nil, "usage": usage}, nil
}

func writeAnthropicStream(w http.ResponseWriter, body []byte, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	id := "msg_" + randomID()
	writeAnthropicEvent(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})
	blockStarted := false
	stopReason := "end_turn"
	if !strings.Contains(string(body), "data:") {
		if message, err := chatToAnthropicResponse(body, model); err == nil {
			if blocks, ok := message["content"].([]map[string]any); ok {
				for _, block := range blocks {
					if text, ok := block["text"].(string); ok && text != "" {
						writeAnthropicEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
						writeAnthropicEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
						writeAnthropicEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
						break
					}
				}
			}
			if reason, ok := message["stop_reason"].(string); ok && reason != "" {
				stopReason = reason
			}
		}
		writeAnthropicEvent(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 0}})
		writeAnthropicEvent(w, "message_stop", map[string]any{"type": "message_stop"})
		return
	}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.TrimSpace(strings.TrimPrefix(line, "data: ")) == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if text, ok := delta["content"].(string); ok && text != "" {
			if !blockStarted {
				writeAnthropicEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
				blockStarted = true
			}
			writeAnthropicEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
		}
		if finish, _ := choice["finish_reason"].(string); finish != "" {
			if blockStarted {
				writeAnthropicEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
			}
			if finish == "length" {
				stopReason = "max_tokens"
			} else if finish == "tool_calls" {
				stopReason = "tool_use"
			}
		}
	}
	writeAnthropicEvent(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 0}})
	writeAnthropicEvent(w, "message_stop", map[string]any{"type": "message_stop"})
}

func writeAnthropicEvent(w http.ResponseWriter, event string, payload map[string]any) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeAnthropicProxyError(w http.ResponseWriter, status int, body []byte) {
	message := "upstream request failed"
	var openAI struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &openAI) == nil && openAI.Error.Message != "" {
		message = openAI.Error.Message
	}
	if status < 400 {
		status = http.StatusBadGateway
	}
	writeAnthropicError(w, status, "api_error", message)
}

func writeAnthropicError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": errorType, "message": message}})
}
