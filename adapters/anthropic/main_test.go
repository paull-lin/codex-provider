package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func fakePool(t *testing.T, fn func(*http.Request) (int, string)) (*httptest.Server, *adapter) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, body := fn(r)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, &adapter{poolURL: server.URL, originator: "codex_exec", userAgent: "codex_exec/0.163.0 (test)", client: server.Client()}
}

func callMessages(t *testing.T, a *adapter, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.messages(rec, req)
	return rec
}

const plainSSE = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_plain\"}}\n\nevent: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\"}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_plain\",\"usage\":{\"input_tokens\":10,\"input_tokens_details\":{\"cached_tokens\":4},\"output_tokens\":2}}}\n\n"

func TestPlainTextStreamAndForwardedKey(t *testing.T) {
	var got map[string]any
	var auth string
	server, a := fakePool(t, func(r *http.Request) (int, string) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		auth = r.Header.Get("Authorization")
		return 200, plainSSE
	})
	defer server.Close()
	rec := callMessages(t, a, `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}],"stream":true}`, map[string]string{"x-api-key": "secret-key", "anthropic-version": "2023-06-01"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "text_delta") || !strings.Contains(rec.Body.String(), "message_stop") {
		t.Fatalf("stream response = %d %s", rec.Code, rec.Body.String())
	}
	if auth != "Bearer secret-key" {
		t.Fatalf("authorization = %q", auth)
	}
	if got["model"] != "claude-sonnet-4" || got["stream"] != true || got["store"] != false {
		t.Fatalf("mapped request = %#v", got)
	}
}

func TestToolRoundTripAndReasoningRestore(t *testing.T) {
	var got map[string]any
	server, a := fakePool(t, func(r *http.Request) (int, string) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		return 200, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\nevent: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"call_1\",\"call_id\":\"call_1\",\"name\":\"lookup\"}}\n\nevent: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"q\\\":\\\"x\\\"}\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\"}}\n\n"
	})
	defer server.Close()
	encoded := base64.StdEncoding.EncodeToString([]byte(`{"id":"reason_1","encrypted_content":"enc-data"}`))
	body := `{"model":"claude-opus-4","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hidden","signature":"cxr1:` + encoded + `"},{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"result","is_error":false}]}],"tools":[{"name":"lookup","description":"find","input_schema":{"type":"object"}}],"stream":true}`
	rec := callMessages(t, a, body, map[string]string{"Authorization": "Bearer test-k"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "tool_use") || !strings.Contains(rec.Body.String(), "input_json_delta") {
		t.Fatalf("tool stream = %d %s", rec.Code, rec.Body.String())
	}
	items, _ := got["input"].([]any)
	raw, _ := json.Marshal(items)
	text := string(raw)
	if !strings.Contains(text, `"type":"reasoning"`) || !strings.Contains(text, `"encrypted_content":"enc-data"`) || !strings.Contains(text, `function_call_output`) {
		t.Fatalf("input mapping = %s", text)
	}
}

func TestThinkingSignatureRoundTrip(t *testing.T) {
	server, a := fakePool(t, func(r *http.Request) (int, string) {
		return 200, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\nevent: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"reason_1\"}}\n\nevent: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"delta\":\"plan\"}\n\nevent: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"reason_1\",\"encrypted_content\":\"enc-data\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\"}}\n\n"
	})
	defer server.Close()
	rec := callMessages(t, a, `{"model":"claude-opus-4","messages":[{"role":"user","content":"hi"}],"stream":true}`, map[string]string{"Authorization": "Bearer test-k"})
	if !strings.Contains(rec.Body.String(), "thinking_delta") || !strings.Contains(rec.Body.String(), "signature_delta") || !strings.Contains(rec.Body.String(), "cxr1:") {
		t.Fatalf("reasoning SSE = %s", rec.Body.String())
	}
}

func TestCapacityErrorBeforeFirstByteAndMidStream(t *testing.T) {
	server, a := fakePool(t, func(r *http.Request) (int, string) {
		return 529, `{"error":{"code":"server_is_overloaded","message":"busy"}}`
	})
	defer server.Close()
	rec := callMessages(t, a, `{"model":"claude-haiku","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"Authorization": "Bearer test-k"})
	if rec.Code != 529 || !strings.Contains(rec.Body.String(), "overloaded_error") {
		t.Fatalf("capacity response = %d %s", rec.Code, rec.Body.String())
	}
	server2, a2 := fakePool(t, func(r *http.Request) (int, string) {
		return 200, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\nevent: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\"}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"partial\"}\n\nevent: error\ndata: {\"type\":\"error\",\"code\":\"server_is_overloaded\",\"message\":\"busy\"}\n\n"
	})
	defer server2.Close()
	rec = callMessages(t, a2, `{"model":"claude-haiku","messages":[{"role":"user","content":"hi"}],"stream":true}`, map[string]string{"Authorization": "Bearer test-k"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Fatalf("midstream response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestNonStreamAggregationAndStableSessionHeaders(t *testing.T) {
	var mu sync.Mutex
	var sessions []string
	server, a := fakePool(t, func(r *http.Request) (int, string) {
		mu.Lock()
		sessions = append(sessions, r.Header.Get("Session-Id")+"|"+r.Header.Get("Thread-Id")+"|"+r.Header.Get("X-Client-Request-Id"))
		mu.Unlock()
		return 200, plainSSE
	})
	defer server.Close()
	body := `{"model":"claude-sonnet","messages":[{"role":"user","content":"hi"}],"stream":false}`
	for i := 0; i < 2; i++ {
		rec := callMessages(t, a, body, map[string]string{"Authorization": "Bearer test-k", "x-claude-code-session-id": "conversation-1", "x-stainless-retry-count": "9"})
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"type":"message"`) || !strings.Contains(rec.Body.String(), "hello") {
			t.Fatalf("aggregate = %d %s", rec.Code, rec.Body.String())
		}
	}
	if len(sessions) != 2 || sessions[0] != sessions[1] {
		t.Fatalf("session headers = %#v", sessions)
	}
}
