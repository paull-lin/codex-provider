package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func recorded(events ...string) string {
	var out strings.Builder
	for _, event := range events {
		out.WriteString("data: ")
		out.WriteString(event)
		out.WriteString("\n\n")
	}
	return out.String()
}

const completed = `{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":60},"output_tokens":12}}}`

var mixedSSE = recorded(
	`{"type":"response.created","response":{"id":"resp_1"}}`,
	`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
	`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"Check "}`,
	`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"the file."}`,
	`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"text":"Check the file."}],"encrypted_content":"opaque-reasoning"}}`,
	`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"m_1"}}`,
	`{"type":"response.output_text.delta","output_index":1,"delta":"Looking now."}`,
	`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"m_1","content":[{"type":"output_text","text":"Looking now."}]}}`,
	`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file"}}`,
	`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"path\":"}`,
	`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"\"main.go\"}"}`,
	`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"main.go\"}"}}`,
	completed,
)

func decoded(t *testing.T, raw string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, raw)
	}
	return value
}
func sseEvents(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "data: ") {
			events = append(events, decoded(t, strings.TrimPrefix(line, "data: ")))
		}
	}
	return events
}
func requireUsage(t *testing.T, value any) {
	t.Helper()
	want := map[string]any{"input_tokens": float64(40), "cache_read_input_tokens": float64(60), "output_tokens": float64(12)}
	if !reflect.DeepEqual(value, want) {
		t.Fatalf("usage=%#v, want %#v", value, want)
	}
}

func TestMixedStreamAndNonStreamParity(t *testing.T) {
	_, a := fakePool(t, func(*http.Request) (int, string) { return 200, mixedSSE })
	r := callMessages(t, a, `{"model":"claude-sonnet-test","stream":true,"messages":[{"role":"user","content":"read"}]}`, map[string]string{"x-api-key": "test-fixture-key"})
	if r.Code != 200 || r.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	events := sseEvents(t, r.Body.String())
	var starts, stops []int
	var signature, args, thinking, text string
	for _, e := range events {
		switch e["type"] {
		case "message_start":
			m := e["message"].(map[string]any)
			if m["id"] != "resp_1" || m["model"] != "claude-sonnet-test" {
				t.Fatal(m)
			}
		case "content_block_start":
			starts = append(starts, int(e["index"].(float64)))
			b := e["content_block"].(map[string]any)
			if b["type"] == "tool_use" && (b["id"] != "call_1" || b["name"] != "read_file") {
				t.Fatal(b)
			}
		case "content_block_stop":
			stops = append(stops, int(e["index"].(float64)))
		case "content_block_delta":
			d := e["delta"].(map[string]any)
			switch d["type"] {
			case "text_delta":
				text += d["text"].(string)
			case "thinking_delta":
				thinking += d["thinking"].(string)
			case "signature_delta":
				signature += d["signature"].(string)
			case "input_json_delta":
				args += d["partial_json"].(string)
			}
		case "message_delta":
			requireUsage(t, e["usage"])
			if e["delta"].(map[string]any)["stop_reason"] != "tool_use" {
				t.Fatal(e)
			}
		}
	}
	if !reflect.DeepEqual(starts, []int{0, 1, 2}) || !reflect.DeepEqual(stops, starts) {
		t.Fatalf("starts=%v stops=%v", starts, stops)
	}
	if thinking != "Check the file." || text != "Looking now." || decoded(t, args)["path"] != "main.go" {
		t.Fatalf("duplicated/missing deltas: %q %q %q", thinking, text, args)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(signature, "cxr1:"))
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded(t, string(raw)); got["id"] != "rs_1" || got["encrypted_content"] != "opaque-reasoning" {
		t.Fatal(got)
	}
	if events[len(events)-1]["type"] != "message_stop" {
		t.Fatal("missing message_stop")
	}

	for _, streamField := range []string{`,"stream":false`, ""} {
		r = callMessages(t, a, `{"model":"claude-sonnet-test","messages":[{"role":"user","content":"read"}]`+streamField+`}`, map[string]string{"Authorization": "Bearer test-fixture-key"})
		if r.Header().Get("Content-Type") != "application/json" {
			t.Fatal(r.Header())
		}
		m := decoded(t, r.Body.String())
		requireUsage(t, m["usage"])
		if m["stop_reason"] != "tool_use" || m["model"] != "claude-sonnet-test" {
			t.Fatal(m)
		}
		content := m["content"].([]any)
		if len(content) != 3 {
			t.Fatal(content)
		}
		if content[0].(map[string]any)["signature"] != signature || content[0].(map[string]any)["thinking"] != thinking || content[1].(map[string]any)["text"] != text || !reflect.DeepEqual(content[2].(map[string]any)["input"], decoded(t, args)) {
			t.Fatal(content)
		}
	}
}

func TestGeneratedSignatureAndToolResultReplay(t *testing.T) {
	requests := make(chan map[string]any, 2)
	_, a := fakePool(t, func(r *http.Request) (int, string) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		requests <- body
		return 200, mixedSSE
	})
	r := callMessages(t, a, `{"model":"claude-opus","messages":[{"role":"user","content":"read"}]}`, map[string]string{"x-api-key": "test-fixture-key"})
	<-requests
	content := decoded(t, r.Body.String())["content"]
	body, _ := json.Marshal(map[string]any{"model": "claude-opus", "messages": []any{
		map[string]any{"role": "user", "content": "read"}, map[string]any{"role": "assistant", "content": content},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_1", "is_error": true, "content": []any{map[string]any{"type": "text", "text": "not found"}}}}},
	}})
	r = callMessages(t, a, string(body), map[string]string{"x-api-key": "test-fixture-key"})
	if r.Code != 200 {
		t.Fatal(r.Body)
	}
	items := (<-requests)["input"].([]any)
	if len(items) != 5 {
		t.Fatal(items)
	}
	reasoning := items[1].(map[string]any)
	if reasoning["type"] != "reasoning" || reasoning["id"] != "rs_1" || reasoning["encrypted_content"] != "opaque-reasoning" {
		t.Fatal(reasoning)
	}
	tool := items[3].(map[string]any)
	if tool["call_id"] != "call_1" || decoded(t, tool["arguments"].(string))["path"] != "main.go" {
		t.Fatal(tool)
	}
	result := items[4].(map[string]any)
	if result["type"] != "function_call_output" || result["call_id"] != "call_1" || result["output"] != "[tool_result is_error=true]\nnot found" {
		t.Fatal(result)
	}
}

func TestHTTPAndStreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   int
		kind   string
	}{
		{"http capacity", 429, `{"error":{"code":"server_is_overloaded"}}`, 529, "overloaded_error"},
		{"http rate", 429, `{"error":{"code":"rate_limit_exceeded"}}`, 429, "rate_limit_error"},
		{"http auth", 401, `{"error":{"message":"bad key"}}`, 401, "authentication_error"},
		{"http request", 400, `{}`, 400, "invalid_request_error"},
		{"http server", 500, `private upstream text`, 502, "api_error"},
		{"first event", 200, recorded(`{"type":"error","error":{"code":"capacity"}}`), 529, "overloaded_error"},
		{"lifecycle then failed", 200, ": keepalive\n\n" + recorded(`{"type":"response.created","response":{"id":"r"}}`, `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}`), 529, "overloaded_error"},
		{"wrapped status", 200, recorded(`{"type":"error","status":401,"error":{"message":"failed"}}`), 401, "authentication_error"},
		{"native auth", 200, recorded(`{"type":"error","code":"account_auth_failed"}`), 401, "authentication_error"},
		{"native prompt", 200, recorded(`{"type":"error","code":"context_length_exceeded"}`), 400, "invalid_request_error"},
		{"native slow down", 200, recorded(`{"type":"error","code":"slow_down"}`), 529, "overloaded_error"},
		{"eof", 200, recorded(`{"type":"response.created","response":{"id":"r"}}`), 502, "api_error"},
		{"malformed", 200, "data: not JSON\n\n", 502, "api_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, a := fakePool(t, func(*http.Request) (int, string) { return tc.status, tc.body })
			for _, stream := range []bool{true, false} {
				r := callMessages(t, a, fmt.Sprintf(`{"model":"claude-sonnet","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream), map[string]string{"x-api-key": "test-fixture-key"})
				if r.Code != tc.want || r.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("status=%d %s", r.Code, r.Body)
				}
				e := decoded(t, r.Body.String())
				if e["type"] != "error" || e["error"].(map[string]any)["type"] != tc.kind {
					t.Fatal(e)
				}
			}
		})
	}
	for _, ending := range []string{recorded(`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}`), ""} {
		_, a := fakePool(t, func(*http.Request) (int, string) {
			return 200, recorded(`{"type":"response.output_text.delta","output_index":0,"delta":"partial"}`) + ending
		})
		for _, stream := range []bool{true, false} {
			r := callMessages(t, a, fmt.Sprintf(`{"model":"claude-sonnet","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream), map[string]string{"x-api-key": "test-fixture-key"})
			if stream {
				events := sseEvents(t, r.Body.String())
				if r.Code != 200 || events[len(events)-1]["type"] != "error" || strings.Contains(r.Body.String(), "message_stop") {
					t.Fatal(r.Body)
				}
			} else if r.Code < 400 || r.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("nonstream failure %d %s", r.Code, r.Body)
			}
		}
	}
}

func TestHeadersAndSessionAcrossTurns(t *testing.T) {
	type capture struct {
		header http.Header
		body   map[string]any
	}
	requests := make(chan capture, 4)
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/responses" {
			t.Errorf("upstream target: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		requests <- capture{r.Header.Clone(), body}
		_, _ = io.WriteString(w, plainSSE)
	}))
	defer pool.Close()
	t.Setenv("ANTHROPIC_ADAPTER_POOL_URL", pool.URL+"/")
	t.Setenv("ANTHROPIC_ADAPTER_ORIGINATOR", "configured-originator")
	t.Setenv("ANTHROPIC_ADAPTER_USER_AGENT", "configured-agent")
	a, err := newAdapter()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		headers  map[string]string
		metadata string
		want     string
	}{
		{"header", map[string]string{"x-claude-code-session-id": "conversation-id"}, `,"metadata":{"user_id":"other-id"}`, "conversation-id"},
		{"metadata", nil, `,"metadata":{"user_id":"metadata-id"}`, "metadata-id"},
		{"hash", nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var previous string
			for _, extra := range []string{"", `,{"role":"assistant","content":"hi"},{"role":"user","content":"next"}`} {
				headers := map[string]string{"x-api-key": "test-fixture-key", "Authorization": "Bearer test-ignored-key", "anthropic-version": "2023-06-01", "anthropic-beta": "test", "x-stainless-retry-count": "3", "x-app": "cli", "User-Agent": "claude-client", "Cookie": "test-private-cookie"}
				for k, v := range tc.headers {
					headers[k] = v
				}
				r := callMessages(t, a, `{"model":"claude-sonnet-4","system":"system","messages":[{"role":"user","content":"first"}`+extra+`]`+tc.metadata+`}`, headers)
				if r.Code != 200 {
					t.Fatal(r.Body)
				}
				got := <-requests
				session := got.header.Get("Session-Id")
				if session == "" || got.header.Get("Thread-Id") != session || got.header.Get("X-Client-Request-Id") != session || got.body["prompt_cache_key"] != session {
					t.Fatal(got)
				}
				if (previous != "" && session != previous) || (tc.want != "" && session != tc.want) {
					t.Fatalf("unstable session: %q %q", previous, session)
				}
				previous = session
				if got.header.Get("Authorization") != "Bearer test-fixture-key" || got.header.Get("User-Agent") != "configured-agent" || got.header.Get("Originator") != "configured-originator" {
					t.Fatal("configured headers/key not forwarded")
				}
				for key := range got.header {
					lower := strings.ToLower(key)
					if strings.HasPrefix(lower, "anthropic-") || strings.HasPrefix(lower, "x-stainless-") || lower == "x-app" || lower == "x-api-key" || lower == "cookie" || lower == "x-claude-code-session-id" {
						t.Fatalf("leaked header %s", key)
					}
				}
				if got.body["model"] != "gpt-6.1-sol" {
					t.Fatal(got.body)
				}
			}
		})
	}
}

func TestRequestMappingAndConfiguration(t *testing.T) {
	var req anthropicRequest
	raw := `{"model":"claude-opus","max_tokens":17,"system":[{"type":"text","text":"one"},{"type":"text","text":"two"}],"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]},{"role":"assistant","content":[{"type":"thinking","thinking":"foreign","signature":"foreign"},{"type":"redacted_thinking","data":"drop"},{"type":"text","text":"answer"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}],"thinking":{"type":"enabled","budget_tokens":12000},"tools":[{"name":"read","input_schema":{"type":"object"}}]}`
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	input, err := mapRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(input)
	items := decoded(t, `{"items":`+string(encoded)+`}`)["items"].([]any)
	if len(items) != 4 {
		t.Fatal(items)
	}
	developer := items[0].(map[string]any)
	if developer["role"] != "developer" || len(developer["content"].([]any)) != 2 {
		t.Fatal(developer)
	}
	user := items[1].(map[string]any)["content"].([]any)
	if len(user) != 2 || user[1].(map[string]any)["image_url"] != "data:image/png;base64,aGk=" {
		t.Fatal(user)
	}
	if items[2].(map[string]any)["content"].([]any)[0].(map[string]any)["type"] != "output_text" || items[3].(map[string]any)["output"] != "ok" {
		t.Fatal(items)
	}
	if requestEffort(req) != "medium" {
		t.Fatal("budget mapping")
	}
	for _, tc := range []struct {
		raw  string
		want any
	}{
		{`{"type":"auto"}`, "auto"}, {`{"type":"any"}`, "required"}, {`{"type":"none"}`, "none"}, {`{"type":"tool","name":"read"}`, map[string]any{"type": "function", "name": "read"}},
	} {
		got, err := mapToolChoice(json.RawMessage(tc.raw))
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("choice=%#v %v", got, err)
		}
	}
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{`{"thinking":{"type":"enabled","budget_tokens":1024}}`, "low"},
		{`{"thinking":{"type":"enabled","budget_tokens":16000}}`, "high"},
		{`{"thinking":{"type":"disabled","budget_tokens":16000}}`, ""},
		{`{"thinking":{"type":"enabled","budget_tokens":16000},"output_config":{"effort":"low"}}`, "low"},
		{`{"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`, "high"},
	} {
		var req anthropicRequest
		_ = json.Unmarshal([]byte(tc.raw), &req)
		if got := requestEffort(req); got != tc.want {
			t.Fatalf("effort=%s want %s", got, tc.want)
		}
	}
	requests := make(chan map[string]any, 1)
	_, a := fakePool(t, func(r *http.Request) (int, string) {
		var v map[string]any
		_ = json.NewDecoder(r.Body).Decode(&v)
		requests <- v
		return 200, plainSSE
	})
	r := callMessages(t, a, raw, map[string]string{"Authorization": "Bearer test-fixture-key"})
	if r.Code != 200 {
		t.Fatal(r.Body)
	}
	v := <-requests
	if _, ok := v["max_output_tokens"]; ok {
		t.Fatal("unsupported max_output_tokens")
	}
	if v["parallel_tool_calls"] != true || v["tool_choice"] != "auto" || v["text"].(map[string]any)["verbosity"] != "low" || v["reasoning"].(map[string]any)["summary"] != "auto" {
		t.Fatal(v)
	}
	tool := v["tools"].([]any)[0].(map[string]any)
	if tool["strict"] != false || tool["type"] != "function" || tool["parameters"] == nil {
		t.Fatal(tool)
	}
	if !reflect.DeepEqual(v["include"], []any{"reasoning.encrypted_content"}) {
		t.Fatal(v)
	}
	rules, err := parseModelMap("exact=gpt-a,prefix*=gpt-b,default=gpt-c")
	if err != nil {
		t.Fatal(err)
	}
	a.modelMap = rules
	for model, want := range map[string]string{"exact": "gpt-a", "exact-other": "gpt-c", "prefix-other": "gpt-b", "unknown": "gpt-c"} {
		if a.mapModel(model) != want {
			t.Fatal(model)
		}
	}
}

func TestCountTokensAndHealth(t *testing.T) {
	a := &adapter{}
	r := httptest.NewRecorder()
	a.handler().ServeHTTP(r, httptest.NewRequest("GET", "/healthz", nil))
	if r.Code != 200 {
		t.Fatal(r.Code)
	}
	var previous float64
	for _, body := range []string{
		`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"claude","system":"a longer system prompt","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"a much longer reply"}],"tools":[{"name":"read","input_schema":{"type":"object"}}]}`,
	} {
		req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
		req.Header.Set("x-api-key", "test-fixture-key")
		r := httptest.NewRecorder()
		a.handler().ServeHTTP(r, req)
		tokens := decoded(t, r.Body.String())["input_tokens"].(float64)
		if tokens <= previous {
			t.Fatalf("estimate did not include full input: %v %v", previous, tokens)
		}
		previous = tokens
	}
}

func TestEmptyReasoningAndIncomplete(t *testing.T) {
	_, a := fakePool(t, func(*http.Request) (int, string) {
		return 200, recorded(
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_empty","type":"reasoning","summary":[],"encrypted_content":"encrypted-only"}}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","content":[{"type":"output_text","text":"partial"}]}}`,
			`{"type":"response.incomplete","response":{"id":"r","incomplete_details":{"reason":"max_output_tokens"}}}`,
		)
	})
	r := callMessages(t, a, `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"x-api-key": "test-fixture-key"})
	m := decoded(t, r.Body.String())
	if m["stop_reason"] != "max_tokens" {
		t.Fatal(m)
	}
	content := m["content"].([]any)
	if len(content) != 2 || content[0].(map[string]any)["thinking"] != "" || content[0].(map[string]any)["signature"] == "" {
		t.Fatal(content)
	}
}

func nextWireEvent(t *testing.T, scanner *bufio.Scanner) map[string]any {
	t.Helper()
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			return decoded(t, strings.TrimPrefix(scanner.Text(), "data: "))
		}
	}
	t.Fatalf("stream ended: %v", scanner.Err())
	return nil
}

func TestIncrementalDeliveryAndCancellation(t *testing.T) {
	release := make(chan struct{})
	cancelled := make(chan struct{})
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, recorded(`{"type":"response.output_text.delta","output_index":0,"delta":"first"}`))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			close(cancelled)
			return
		}
		_, _ = io.WriteString(w, recorded(completed))
	}))
	defer pool.Close()
	a := &adapter{poolURL: pool.URL, client: pool.Client()}
	server := httptest.NewServer(a.handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/messages", strings.NewReader(`{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "test-fixture-key")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(response.Body)
	for nextWireEvent(t, scanner)["type"] != "content_block_delta" {
	}
	// The pool is still blocked, proving the text arrived before completion.
	_ = response.Body.Close()
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("upstream did not observe client cancellation")
	}
}

func TestPingWhileWaitingThenFailure(t *testing.T) {
	release := make(chan struct{})
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, recorded(`{"type":"error","code":"server_is_overloaded"}`))
	}))
	defer pool.Close()
	a := &adapter{poolURL: pool.URL, client: pool.Client(), pingInterval: 10 * time.Millisecond}
	server := httptest.NewServer(a.handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/messages", strings.NewReader(`{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "test-fixture-key")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	if nextWireEvent(t, scanner)["type"] != "message_start" {
		t.Fatal("missing start")
	}
	if nextWireEvent(t, scanner)["type"] != "ping" {
		t.Fatal("missing heartbeat")
	}
	close(release)
	for {
		e := nextWireEvent(t, scanner)
		if e["type"] == "error" {
			if e["error"].(map[string]any)["type"] != "overloaded_error" {
				t.Fatal(e)
			}
			break
		}
	}
}
