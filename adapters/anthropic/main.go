package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultModelMap = "claude-opus*=gpt-6-astra,claude-sonnet*=gpt-6.1-sol,claude-haiku*=gpt-5.6-luna,default=gpt-6.1-sol"

type adapter struct {
	poolURL, originator, userAgent string
	modelMap                       []modelRule
	client                         *http.Client
	pingInterval                   time.Duration
}
type modelRule struct{ pattern, model string }

func env(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func newAdapter() (*adapter, error) {
	base := strings.TrimRight(env("ANTHROPIC_ADAPTER_POOL_URL", "http://127.0.0.1:8317"), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("ANTHROPIC_ADAPTER_POOL_URL must be an HTTP(S) base URL without credentials, query or fragment")
	}
	rules, err := parseModelMap(env("ANTHROPIC_ADAPTER_MODEL_MAP", defaultModelMap))
	if err != nil {
		return nil, err
	}
	return &adapter{
		poolURL: base + "/v1/responses", modelMap: rules,
		originator: env("ANTHROPIC_ADAPTER_ORIGINATOR", "codex_exec"),
		userAgent:  env("ANTHROPIC_ADAPTER_USER_AGENT", "codex_exec/0.163.0 (Alpine Linux 3.21.7; x86_64) unknown (codex_exec; 0.163.0)"),
		// A fixed pool destination must not redirect the caller's credential elsewhere.
		client:       &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		pingInterval: 15 * time.Second,
	}, nil
}

func parseModelMap(raw string) ([]modelRule, error) {
	var rules []modelRule
	for _, entry := range strings.Split(raw, ",") {
		pattern, model, ok := strings.Cut(strings.TrimSpace(entry), "=")
		pattern, model = strings.TrimSpace(pattern), strings.TrimSpace(model)
		if !ok || pattern == "" || model == "" || strings.ContainsAny(strings.TrimSuffix(pattern, "*"), "*?[") {
			return nil, fmt.Errorf("ANTHROPIC_ADAPTER_MODEL_MAP requires name=model or prefix*=model entries")
		}
		rules = append(rules, modelRule{pattern, model})
	}
	return rules, nil
}

func (a *adapter) mapModel(name string) string {
	if len(a.modelMap) == 0 {
		return name
	}
	fallback := "gpt-6.1-sol"
	for _, rule := range a.modelMap {
		if rule.pattern == "default" || rule.pattern == "*" {
			fallback = rule.model
			continue
		}
		if name == rule.pattern || (strings.HasSuffix(rule.pattern, "*") && strings.HasPrefix(name, strings.TrimSuffix(rule.pattern, "*"))) {
			return rule.model
		}
	}
	return fallback
}

func (a *adapter) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /v1/messages", a.messages)
	mux.HandleFunc("POST /v1/messages/count_tokens", a.countTokens)
	return mux
}

func main() {
	a, err := newAdapter()
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: env("ANTHROPIC_ADAPTER_ADDR", ":8400"), Handler: a.handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Print("anthropic adapter listening")
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func bearer(r *http.Request) string {
	if key := strings.TrimSpace(r.Header.Get("X-Api-Key")); key != "" {
		return key
	}
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

func decodeRequest(w http.ResponseWriter, r *http.Request) (anthropicRequest, error) {
	var req anthropicRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20))
	if err := decoder.Decode(&req); err != nil {
		return req, fmt.Errorf("invalid or oversized JSON request")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return req, fmt.Errorf("expected a single JSON request")
	}
	if req.Model == "" || len(req.Messages) == 0 {
		return req, fmt.Errorf("model and messages are required")
	}
	return req, nil
}

func (a *adapter) countTokens(w http.ResponseWriter, r *http.Request) {
	if bearer(r) == "" {
		writeAnthropicError(w, 401, "authentication_error", "missing API key")
		return
	}
	req, err := decodeRequest(w, r)
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	// This local estimate performs no inference and cannot validate a pool key.
	// Include the entire conversation and tool schemas, not just the session seed.
	raw, _ := json.Marshal(struct {
		System   json.RawMessage    `json:"system"`
		Messages []anthropicMessage `json:"messages"`
		Tools    []anthropicTool    `json:"tools,omitempty"`
	}{req.System, req.Messages, req.Tools})
	writeJSON(w, 200, map[string]int{"input_tokens": max(1, (len(raw)+3)/4)})
}

func (a *adapter) messages(w http.ResponseWriter, r *http.Request) {
	key := bearer(r)
	if key == "" {
		writeAnthropicError(w, 401, "authentication_error", "missing API key")
		return
	}
	req, err := decodeRequest(w, r)
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	input, err := mapRequest(req)
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	choice, err := mapToolChoice(req.ToolChoice)
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	tools, err := mapTools(req.Tools)
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	session := sessionID(r, req)
	// Codex rejects max_output_tokens; the pool strips it even on native-shaped
	// requests. Do not imply that Anthropic max_tokens is enforced here.
	body := map[string]any{"model": a.mapModel(req.Model), "input": input, "stream": true, "store": false, "include": []string{"reasoning.encrypted_content"}, "parallel_tool_calls": true, "prompt_cache_key": session}
	if effort := requestEffort(req); effort != "" {
		body["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if choice != nil {
		body["tool_choice"] = choice
	}
	payload, err := json.Marshal(body)
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", "cannot encode request")
		return
	}
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, a.poolURL, bytes.NewReader(payload))
	if err != nil {
		writeAnthropicError(w, 502, "api_error", "cannot create upstream request")
		return
	}
	// Only this allowlist crosses the boundary. Never copy Anthropic headers,
	// client UA, cookies or credentials other than the selected pool bearer key.
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "text/event-stream")
	upReq.Header.Set("Authorization", "Bearer "+key)
	for _, name := range []string{"session-id", "thread-id", "x-client-request-id"} {
		upReq.Header.Set(name, session)
	}
	upReq.Header.Set("Originator", a.originator)
	upReq.Header.Set("User-Agent", a.userAgent)
	resp, err := a.client.Do(upReq)
	if err != nil {
		writeAnthropicError(w, 502, "api_error", "upstream unavailable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		e := upstreamError(resp.StatusCode, raw)
		writeAnthropicError(w, e.status, e.kind, e.message)
		return
	}
	a.relay(w, r, resp.Body, req.Model, req.Stream)
}

func writeAnthropicError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
