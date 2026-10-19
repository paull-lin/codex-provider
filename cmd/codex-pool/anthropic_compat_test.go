package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicMessagesTranslatesToChatAndBack(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("upstream path = %s", r.URL.Path)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["model"] != "gpt-test" {
			t.Fatalf("upstream model = %#v", request["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test","output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":12,"output_tokens":3}}`))
	}))
	defer upstream.Close()
	a := testApp(t, []account{{ID: "only", AuthType: "provider_api_key", Enabled: true, InPool: true, Priority: 100, UpstreamBaseURL: upstream.URL, AllowedModels: []string{"gpt-test"}}})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-test","max_tokens":32,"system":"Be concise.","messages":[{"role":"user","content":[{"type":"text","text":"Reply OK."}]}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	recorder := httptest.NewRecorder()
	a.publicMux().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["type"] != "message" || response["role"] != "assistant" {
		t.Fatalf("unexpected response: %#v", response)
	}
	content := response["content"].([]any)
	if content[0].(map[string]any)["text"] != "OK" {
		t.Fatalf("content = %#v", content)
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(12) || usage["output_tokens"] != float64(3) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestAnthropicMessagesStreamTranslatesSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test","output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":12,"output_tokens":3}}`))
	}))
	defer upstream.Close()
	a := testApp(t, []account{{ID: "only", AuthType: "provider_api_key", Enabled: true, InPool: true, Priority: 100, UpstreamBaseURL: upstream.URL, AllowedModels: []string{"gpt-test"}}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-test","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"Reply OK."}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	recorder := httptest.NewRecorder()
	a.publicMux().ServeHTTP(recorder, req)
	output := recorder.Body.String()
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status/content-type = %d/%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), output)
	}
	for _, event := range []string{"event: message_start", "event: content_block_delta", `"text":"OK"`, "event: message_delta", "event: message_stop"} {
		if !strings.Contains(output, event) {
			t.Fatalf("stream missing %q: %s", event, output)
		}
	}
}

func TestAnthropicCountTokensReturnsDeterministicEstimate(t *testing.T) {
	a := testApp(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	recorder := httptest.NewRecorder()
	a.publicMux().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["input_tokens"].(float64) <= 0 {
		t.Fatalf("input_tokens = %#v", response["input_tokens"])
	}
}

func TestAnthropicToolBlocksTranslateToChatToolCalls(t *testing.T) {
	request := map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call-1", "name": "shell", "input": map[string]any{"command": "pwd"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": "ok"}}},
		},
	}
	converted, err := anthropicToChatRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	messages := converted["messages"].([]map[string]any)
	assistant := messages[0]
	calls := assistant["tool_calls"].([]map[string]any)
	if calls[0]["id"] != "call-1" {
		t.Fatalf("tool call = %#v", calls[0])
	}
	if messages[1]["role"] != "tool" || messages[1]["tool_call_id"] != "call-1" {
		t.Fatalf("tool result = %#v", messages[1])
	}
}
