package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestWebSearchRequest(t *testing.T) {
	for _, version := range []string{"web_search_20250305", "web_search_20260209", "web_search_future"} {
		for _, options := range []string{"", `,"allowed_domains":["example.com"],"user_location":{"type":"approximate","city":"Taipei","country":"TW"}`} {
			t.Run(version+options, func(t *testing.T) {
				requests := make(chan map[string]any, 1)
				_, a := fakePool(t, func(r *http.Request) (int, string) {
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					requests <- body
					return 200, plainSSE
				})
				body := fmt.Sprintf(`{"model":"claude-test","max_tokens":128000,"stream":true,"tools":[{"type":%q,"name":"web_search","max_uses":8,"blocked_domains":["blocked.example"]%s},{"name":"read","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"web_search"},"messages":[{"role":"assistant","content":[{"type":"text","text":"before"},{"type":"server_tool_use","id":"srvtoolu_ws_1","name":"web_search","input":{"query":"x"}},{"type":"web_search_tool_result","tool_use_id":"srvtoolu_ws_1","content":[]},{"type":"text","text":"after"}]},{"role":"user","content":"search again"}]}`, version, options)
				r := callMessages(t, a, body, map[string]string{"x-api-key": "test-fixture-key"})
				if r.Code != 200 {
					t.Fatalf("%d %s", r.Code, r.Body)
				}
				got := <-requests
				want := decoded(t, `{"type":"web_search","external_web_access":true,"search_content_types":["text","image"]}`)
				if options != "" {
					want["filters"] = decoded(t, `{"allowed_domains":["example.com"]}`)
					want["user_location"] = decoded(t, `{"type":"approximate","city":"Taipei","country":"TW"}`)
				}
				tools := got["tools"].([]any)
				if !reflect.DeepEqual(tools[0], want) || !reflect.DeepEqual(tools[1], decoded(t, `{"type":"function","name":"read","description":"","parameters":{"type":"object"},"strict":false}`)) {
					t.Fatal(tools)
				}
				if got["tool_choice"] != "auto" || !reflect.DeepEqual(got["include"], []any{"reasoning.encrypted_content"}) {
					t.Fatal(got)
				}
				wantInput := decoded(t, `{"input":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"before"},{"type":"output_text","text":"after"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"search again"}]}]}`)["input"]
				if !reflect.DeepEqual(got["input"], wantInput) {
					t.Fatal(got["input"])
				}
			})
		}
	}
	// A custom function named web_search must remain a function, not a hosted tool.
	got, err := mapToolChoice(json.RawMessage(`{"type":"tool","name":"web_search"}`), []anthropicTool{{Name: "web_search"}})
	if err != nil || !reflect.DeepEqual(got, map[string]any{"type": "function", "name": "web_search"}) {
		t.Fatalf("choice=%v err=%v", got, err)
	}
	_, a := fakePool(t, func(*http.Request) (int, string) {
		t.Error("unsupported server tool reached upstream")
		return 200, plainSSE
	})
	r := callMessages(t, a, `{"model":"claude-test","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"code_execution_20250522","name":"code_execution"}]}`, map[string]string{"x-api-key": "test-fixture-key"})
	if r.Code != 400 || !strings.Contains(r.Body.String(), "invalid_request_error") || !strings.Contains(r.Body.String(), "only custom tools with name and input_schema are supported") {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}

// The base item is copied verbatim from rust-v0.163.0
// codex-rs/protocol/src/models.rs, WebSearchCall's SSE example:
// "action": {"type":"search","query":"weather: San Francisco, CA"}
// Optional/multiple queries follow WebSearchAction in the same source. Sources,
// annotations and failed status below are synthetic defensive-translation cases,
// not a claim that live ChatGPT traffic has been captured by this test.
const searchItem = `{"id":"ws_...","type":"web_search_call","status":"completed","action":{"type":"search","query":"weather: San Francisco, CA"}}`
const searchCitation = `{"type":"url_citation","url":"https://example.com/weather","title":"Weather","start_index":0,"end_index":7}`
const searchResult = `{"type":"web_search_result","url":"https://example.com/weather","title":"Weather","encrypted_content":"","page_age":null}`

func TestWebSearchStreamAndNonStream(t *testing.T) {
	for _, tc := range []struct {
		name, item, input, results, annotations string
		annotationEvent, terminalAnnotations    bool
	}{
		{"annotation event", searchItem, `{"query":"weather: San Francisco, CA"}`, `[` + searchResult + `]`, "", true, false},
		{"completed message", searchItem, `{"query":"weather: San Francisco, CA"}`, `[` + searchResult + `]`, searchCitation, false, false},
		{"terminal annotations deduplicated", searchItem, `{"query":"weather: San Francisco, CA"}`, `[` + searchResult + `]`, searchCitation + `,` + searchCitation, true, true},
		{"sources take precedence", `{"id":"ws_...","type":"web_search_call","status":"completed","action":{"queries":["first","second"],"sources":[{"type":"url","url":"https://source.example","title":"Source"}]}}`, `{"query":"first"}`, `[{"type":"web_search_result","url":"https://source.example","title":"Source","encrypted_content":"","page_age":null}]`, searchCitation, true, false},
		{"empty sources", `{"id":"ws_...","type":"web_search_call","action":{"query":"q","queries":["other"],"sources":[]}}`, `{"query":"q"}`, `[]`, searchCitation, true, false},
		{"failed", `{"id":"ws_...","type":"web_search_call","status":"failed","action":null}`, `{}`, `{"type":"web_search_tool_result_error","error_code":"unavailable"}`, searchCitation, true, false},
		{"missing action", `{"id":"ws_...","type":"web_search_call","future_field":true}`, `{}`, `[]`, "", false, false},
		{"unknown action", `{"id":"ws_...","type":"web_search_call","action":{"type":"future_action","new_field":{}}}`, `{}`, `[]`, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := `{"id":"m_1","type":"message","content":[{"type":"output_text","text":"Weather","annotations":[` + tc.annotations + `]}]}`
			events := []string{
				`{"type":"response.created","response":{"id":"resp_1"}}`,
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"ws_...","type":"web_search_call","status":"in_progress"}}`,
				`{"type":"response.web_search_call.searching","output_index":0}`, // lifecycle is not another call
				`{"type":"response.output_item.done","output_index":0,"item":` + tc.item + `}`,
				`{"type":"response.output_text.delta","output_index":1,"delta":"Wea"}`,
				`{"type":"response.output_text.delta","output_index":1,"delta":"ther"}`,
			}
			if tc.annotationEvent {
				events = append(events, `{"type":"response.output_text.annotation.added","output_index":1,"content_index":0,"annotation_index":0,"annotation":`+searchCitation+`}`)
			}
			doneMessage := message
			if tc.terminalAnnotations {
				doneMessage = `{"id":"m_1","type":"message","content":[{"type":"output_text","text":"Weather"}]}`
			}
			events = append(events, `{"type":"response.output_item.done","output_index":1,"item":`+doneMessage+`}`)
			events = append(events, `{"type":"response.completed","response":{"id":"resp_1","output":[`+tc.item+`,`+message+`],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":60},"output_tokens":12}}}`)
			_, a := fakePool(t, func(*http.Request) (int, string) { return 200, recorded(events...) })
			wantContent := decoded(t, `{"content":[{"type":"server_tool_use","id":"srvtoolu_ws____","name":"web_search","input":`+tc.input+`},{"type":"text","text":"Weather"},{"type":"web_search_tool_result","tool_use_id":"srvtoolu_ws____","content":`+tc.results+`}]}`)["content"]
			wantUsage := decoded(t, `{"input_tokens":40,"cache_read_input_tokens":60,"output_tokens":12,"server_tool_use":{"web_search_requests":1}}`)
			for _, stream := range []bool{true, false} {
				r := callMessages(t, a, fmt.Sprintf(`{"model":"claude-test","stream":%t,"messages":[{"role":"user","content":"search"}]}`, stream), map[string]string{"x-api-key": "test-fixture-key"})
				if r.Code != 200 {
					t.Fatalf("%d %s", r.Code, r.Body)
				}
				var message map[string]any
				if stream {
					message = collectSearchStream(t, r.Body.String())
				} else {
					message = decoded(t, r.Body.String())
				}
				if !reflect.DeepEqual(message["content"], wantContent) || !reflect.DeepEqual(message["usage"], wantUsage) || message["stop_reason"] != "end_turn" {
					t.Fatalf("stream=%v message=%#v", stream, message)
				}
			}
		})
	}
}

// Check the wire lifecycle independently of aggregation: indices are sequential,
// deltas only target open blocks, and search results are complete start payloads.
func collectSearchStream(t *testing.T, raw string) map[string]any {
	t.Helper()
	c := &collector{}
	open := map[int]string{}
	next := 0
	events := sseEvents(t, raw)
	for _, e := range events {
		kind := e["type"].(string)
		index := 0
		if v, ok := e["index"].(float64); ok {
			index = int(v)
			e["index"] = index
		}
		switch kind {
		case "content_block_start":
			if index != next {
				t.Fatalf("index=%d next=%d", index, next)
			}
			next++
			block := e["content_block"].(map[string]any)
			open[index] = block["type"].(string)
			if open[index] == "server_tool_use" && !reflect.DeepEqual(block["input"], map[string]any{}) {
				t.Fatal("server_tool_use must start with empty input", block)
			}
			if open[index] == "web_search_tool_result" && block["content"] == nil {
				t.Fatal("result missing full content", block)
			}
		case "content_block_delta":
			if open[index] == "" || open[index] == "web_search_tool_result" {
				t.Fatal("invalid delta", e)
			}
			if open[index] == "server_tool_use" && e["delta"].(map[string]any)["type"] != "input_json_delta" {
				t.Fatal("invalid server tool delta", e)
			}
		case "content_block_stop":
			if open[index] == "" {
				t.Fatal("invalid stop", e)
			}
			delete(open, index)
		case "message_delta":
			if len(open) != 0 {
				t.Fatal("unclosed blocks", open)
			}
		case "error":
			t.Fatal(e)
		}
		if err := c.emit(kind, e); err != nil {
			t.Fatal(err)
		}
	}
	if len(events) == 0 || events[len(events)-1]["type"] != "message_stop" {
		t.Fatal("missing message_stop")
	}
	return c.message
}

func TestWebSearchMultipleCallsAndFunction(t *testing.T) {
	stream := recorded(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call"}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"web_search_call","id":"ws_2","action":{"type":"open_page","url":"https://example.com"}}}`,
		`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","call_id":"call_1","name":"read","arguments":"{\"path\":\"file\"}"}}`,
		completed,
	)
	_, a := fakePool(t, func(*http.Request) (int, string) { return 200, stream })
	r := callMessages(t, a, `{"model":"claude-test","stream":true,"messages":[{"role":"user","content":"search"}]}`, map[string]string{"x-api-key": "test-fixture-key"})
	m := collectSearchStream(t, r.Body.String())
	if m["stop_reason"] != "tool_use" || m["usage"].(map[string]any)["server_tool_use"].(map[string]any)["web_search_requests"] != float64(2) {
		t.Fatal(m)
	}
	blocks := m["content"].([]any)
	if len(blocks) != 5 || blocks[1].(map[string]any)["type"] != "tool_use" || blocks[1].(map[string]any)["input"].(map[string]any)["path"] != "file" {
		t.Fatal(blocks)
	}
	for i, useIndex := range []int{2, 0} {
		use := blocks[useIndex].(map[string]any)
		result := blocks[i+3].(map[string]any)
		if use["id"] == "" || use["id"] != result["tool_use_id"] || !reflect.DeepEqual(use["input"], map[string]any{}) {
			t.Fatal(use, result)
		}
	}
}

func TestWebSearchTextRemainsIncremental(t *testing.T) {
	var emitted []map[string]any
	c := newConverter("claude-test", func(_ string, event map[string]any) error {
		emitted = append(emitted, event)
		return nil
	})
	for _, raw := range []string{
		`{"type":"response.output_item.done","output_index":0,"item":` + searchItem + `}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"already visible"}`,
	} {
		var event responseEvent
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			t.Fatal(err)
		}
		if terminal, err := c.step(event); terminal || err != nil {
			t.Fatalf("terminal=%v err=%v", terminal, err)
		}
	}
	last := emitted[len(emitted)-1]
	if last["type"] != "content_block_delta" || last["delta"].(map[string]any)["text"] != "already visible" {
		t.Fatal("text must be emitted before annotations or completion", emitted)
	}
}

func TestWebSearchTerminalOutputOnly(t *testing.T) {
	stream := recorded(`{"type":"response.completed","response":{"id":"resp_1","output":[` + searchItem + `,{"type":"message","content":[{"type":"output_text","text":"Weather","annotations":[` + searchCitation + `]}]}]}}`)
	_, a := fakePool(t, func(*http.Request) (int, string) { return 200, stream })
	r := callMessages(t, a, `{"model":"claude-test","stream":true,"messages":[{"role":"user","content":"search"}]}`, map[string]string{"x-api-key": "test-fixture-key"})
	m := collectSearchStream(t, r.Body.String())
	blocks := m["content"].([]any)
	if len(blocks) != 3 || blocks[0].(map[string]any)["input"].(map[string]any)["query"] != "weather: San Francisco, CA" || blocks[1].(map[string]any)["text"] != "Weather" {
		t.Fatal(blocks)
	}
	results := blocks[2].(map[string]any)["content"].([]any)
	if len(results) != 1 || !reflect.DeepEqual(results[0], decoded(t, searchResult)) {
		t.Fatal(results)
	}
}
