package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The operator-fixed effort must override whatever Claude Code asks for.
func TestFixedReasoningEffortOverridesClient(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies <- body
		_, _ = io.WriteString(w, plainSSE)
	}))
	defer pool.Close()
	t.Setenv("ANTHROPIC_ADAPTER_POOL_URL", pool.URL)
	t.Setenv("ANTHROPIC_ADAPTER_REASONING_EFFORT", "medium")
	a, err := newAdapter()
	if err != nil {
		t.Fatal(err)
	}
	callMessages(t, a, `{"model":"claude-opus-5-5","max_tokens":64,"thinking":{"type":"enabled","budget_tokens":32000},"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"Authorization": "Bearer test-k"})
	got := (<-bodies)["reasoning"].(map[string]any)["effort"]
	if got != "medium" {
		t.Fatalf("effort = %v, want medium", got)
	}
}

func TestInvalidFixedReasoningEffortRejected(t *testing.T) {
	t.Setenv("ANTHROPIC_ADAPTER_REASONING_EFFORT", "extreme")
	if _, err := newAdapter(); err == nil {
		t.Fatal("expected an error for an invalid effort")
	}
}
