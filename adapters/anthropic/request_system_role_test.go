package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// Shape captured from Claude Code 2.1.296: a system message between turns.
func TestInterleavedSystemMessageBecomesDeveloper(t *testing.T) {
	var req anthropicRequest
	raw := `{"model":"claude-opus-5-5","system":[{"type":"text","text":"base"}],"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"hi"},{"type":"text","text":"more"}]},` +
		`{"role":"system","content":[{"type":"text","text":"reminder"}]},` +
		`{"role":"user","content":"again"}]}`
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	input, err := mapRequest(req)
	if err != nil {
		t.Fatalf("mapRequest: %v", err)
	}
	var roles []string
	for _, item := range input {
		if m, ok := item.(map[string]any); ok {
			roles = append(roles, m["role"].(string))
		}
	}
	want := []string{"developer", "user", "developer", "user"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v, want %v", roles, want)
		}
	}
}

// Real upstream rejects replayed reasoning without `summary`
// ("Missing required parameter: input[N].summary"); the fake pool did not.
func TestRestoredReasoningCarriesRequiredSummary(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte(`{"id":"rs_1","encrypted_content":"enc"}`))
	item := restoreReasoning("cxr1:" + payload)
	if item == nil {
		t.Fatal("restoreReasoning returned nil")
	}
	summary, ok := item["summary"].([]any)
	if !ok || summary == nil {
		t.Fatalf("summary = %#v, want empty array", item["summary"])
	}
	if item["encrypted_content"] != "enc" || item["type"] != "reasoning" {
		t.Fatalf("item = %#v", item)
	}
}
