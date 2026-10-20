package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testRole(t *testing.T, a *app, name, controlURL string) {
	t.Helper()
	t.Setenv("CODEX_POOL_ROLE", name)
	t.Setenv("CODEX_POOL_INTERNAL_SECRET", "test-internal-secret")
	t.Setenv("CODEX_POOL_CONTROL_URL", controlURL)
	if err := a.configureRole(); err != nil {
		t.Fatal(err)
	}
}

func roleFixture(t *testing.T) (*app, string) {
	t.Helper()
	a := testApp(t, []account{{ID: "slot", AccountID: "workspace-test", AuthType: "codex_device_auth", Enabled: true, InPool: true, Priority: 100}})
	token := fakeJWT(time.Now().Add(time.Hour))
	home := a.accountCodexHome("slot")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"tokens":{"access_token":%q,"refresh_token":"<example-refresh>","account_id":"workspace-test"}}`, token)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	testRole(t, a, "control", "")
	return a, token
}

func roleRequest(a *app, method, path, secret, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if secret != "" {
		r.Header.Set(internalSecretHeader, secret)
	}
	w := httptest.NewRecorder()
	a.mux().ServeHTTP(w, r)
	return w
}

func roleInternalRequest(a *app, method, path, secret, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if secret != "" {
		r.Header.Set(internalSecretHeader, secret)
	}
	w := httptest.NewRecorder()
	a.controlInternalMux().ServeHTTP(w, r)
	return w
}

func TestRoleParsing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wantError bool
	}{
		{"", false}, {"control", false}, {"connection", false}, {"invalid", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &app{apiKeys: [][]byte{[]byte("client-key")}}
			t.Setenv("CODEX_POOL_ROLE", tc.name)
			t.Setenv("CODEX_POOL_INTERNAL_SECRET", "test-internal-secret")
			t.Setenv("CODEX_POOL_CONTROL_URL", "http://control:8318")
			err := a.configureRole()
			if (err != nil) != tc.wantError {
				t.Fatalf("configureRole error = %v", err)
			}
			if tc.name == "" && a.role != nil {
				t.Fatal("default monolith acquired role behavior")
			}
		})
	}
	for _, secret := range []string{"", "client-key"} {
		a := &app{apiKeys: [][]byte{[]byte("client-key")}}
		t.Setenv("CODEX_POOL_ROLE", "control")
		t.Setenv("CODEX_POOL_INTERNAL_SECRET", secret)
		if a.configureRole() == nil {
			t.Fatal("accepted missing/public internal secret")
		}
	}
	for _, value := range []string{"0", "-1m", "bad"} {
		a := &app{}
		t.Setenv("CODEX_POOL_ROLE", "connection")
		t.Setenv("CODEX_POOL_INTERNAL_SECRET", "test-secret")
		t.Setenv("CODEX_POOL_CONTROL_OFFLINE_MAX", value)
		if a.configureRole() == nil {
			t.Fatal("accepted invalid offline maximum")
		}
	}
}

func TestRoleInternalAuthAndSnapshot(t *testing.T) {
	a, token := roleFixture(t)
	t.Log("fixture")
	for _, path := range []string{"/internal/state", "/internal/credentials/slot", "/internal/credentials/slot/rejected"} {
		method := http.MethodPost
		if path == "/internal/state" {
			method = http.MethodGet
		}
		for _, secret := range []string{"", "wrong", "client-key", a.role.secret} {
			if w := roleRequest(a, method, path, secret, ""); w.Code != http.StatusNotFound {
				t.Fatalf("%s public status %d", path, w.Code)
			}
			if secret == a.role.secret {
				continue
			}
			if w := roleInternalRequest(a, method, path, secret, ""); w.Code != 401 {
				t.Fatalf("%s unauthorized status %d", path, w.Code)
			}
		}
	}
	t.Log("unauthorized done")
	w := roleInternalRequest(a, "GET", "/internal/state", a.role.secret, "")
	t.Log("authorized state returned")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("snapshot status %d", w.Code)
	}
	if strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), "<example-refresh>") {
		t.Fatal("snapshot exposed token")
	}
	first := a.controlSnapshot()
	if first.Version != a.controlSnapshot().Version {
		t.Fatal("unchanged snapshot version changed")
	}
	a.config.Accounts[0].Priority++
	if first.Version == a.controlSnapshot().Version {
		t.Fatal("account change did not version snapshot")
	}
	first = a.controlSnapshot()
	a.state.Quotas["slot"] = quotaSnapshot{AccountID: "slot", UsageUpdatedAt: time.Now()}
	if first.Version == a.controlSnapshot().Version {
		t.Fatal("quota change did not version snapshot")
	}
	detached := a.controlSnapshot()
	detached.Quotas["slot"] = quotaSnapshot{AccountID: "must-not-change-control"}
	detached.Config.ModelAliases["alias"] = "must-not-change-control"
	if a.state.Quotas["slot"].AccountID != "slot" || a.config.ModelAliases["alias"] != "gpt-test" {
		t.Fatal("snapshot shares mutable authority with control")
	}
	first = a.controlSnapshot()
	authFile, err := a.readCodexAuthFile(a.config.Accounts[0])
	if err != nil {
		t.Fatal(err)
	}
	authFile.Tokens.AccessToken = fakeJWT(time.Now().Add(2 * time.Hour))
	if err := writeJSONAtomic(filepath.Join(a.accountCodexHome("slot"), "auth.json"), authFile); err != nil {
		t.Fatal(err)
	}
	if first.Version == a.controlSnapshot().Version {
		t.Fatal("credential expiry change did not version snapshot")
	}
	for _, path := range []string{"/v1/models", "/v1/responses"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+a.role.secret)
		r.Header.Set(internalSecretHeader, a.role.secret)
		w := httptest.NewRecorder()
		a.mux().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("secret accepted on public path %s: %d", path, w.Code)
		}
	}
	a.role.secret = ""
	if roleInternalRequest(a, "GET", "/internal/state", "", "").Code != 401 {
		t.Fatal("empty secret did not fail closed")
	}
	a.role = nil
	if roleRequest(a, "GET", "/internal/state", "test-internal-secret", "").Code != 404 {
		t.Fatal("monolith exposed internal state")
	}
}

func TestControlListenerLifecycle(t *testing.T) {
	a, _ := roleFixture(t)
	if a.role.internalAddress != roleInternalAddressDefault {
		t.Fatalf("default internal address = %q", a.role.internalAddress)
	}
	// Reserve an ephemeral address for the real role startup hook to bind.
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()
	t.Setenv("CODEX_POOL_INTERNAL_ADDR", address)
	if err := a.configureRole(); err != nil {
		t.Fatal(err)
	}
	public, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- a.serveControlUntilStopped(ctx, public) }()
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get("http://" + address + "/internal/state")
		if err == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("internal status without secret = %d", response.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, tc := range []struct {
		address, method, path string
		status                int
	}{
		{public.Addr().String(), "GET", "/internal/state", 404},
		{public.Addr().String(), "POST", "/internal/credentials/slot", 404},
		{public.Addr().String(), "POST", "/internal/credentials/slot/rejected", 404},
		{public.Addr().String(), "GET", "/internal/inflight", 200},
		{address, "GET", "/internal/state", 200},
		{address, "POST", "/internal/credentials/slot", 200},
		{address, "GET", "/internal/inflight", 404},
		{address, "GET", "/admin", 404},
	} {
		req, _ := http.NewRequest(tc.method, "http://"+tc.address+tc.path, nil)
		req.Header.Set(internalSecretHeader, a.role.secret)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != tc.status {
			t.Fatalf("%s %s: status %d, want %d", tc.address, tc.path, response.StatusCode, tc.status)
		}
	}
	// Supplying the broker secret must not bypass the existing drain probe's
	// loopback boundary on the normal listener.
	if roleRequest(a, "GET", "/internal/inflight", a.role.secret, "").Code != 404 {
		t.Fatal("network caller reached inflight probe")
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("control did not stop both listeners")
	}
	for _, address := range []string{address, public.Addr().String()} {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err == nil {
			conn.Close()
			t.Fatalf("listener %s still accepts after shutdown", address)
		}
	}
}

func TestRoleLeaseGatesAndConcurrentRejection(t *testing.T) {
	a, rejected := roleFixture(t)
	for _, flag := range []string{"disabled", "pending", "verification", "job"} {
		original := a.config.Accounts[0]
		switch flag {
		case "disabled":
			a.config.Accounts[0].Enabled = false
		case "pending":
			a.config.Accounts[0].PendingPoolActivation = true
		case "verification":
			a.config.Accounts[0].PendingAuthVerification = true
		case "job":
			a.jobs["job"] = &loginJob{AccountID: "slot", Status: "running"}
		}
		for _, suffix := range []string{"", "/rejected"} {
			if w := roleInternalRequest(a, "POST", "/internal/credentials/slot"+suffix, a.role.secret, `{"access_token":"<example-access>"}`); w.Code != 409 {
				t.Fatalf("%s lease status %d", flag, w.Code)
			}
		}
		a.config.Accounts[0] = original
		delete(a.jobs, "job")
	}
	var calls atomic.Int32
	newToken := fakeJWT(time.Now().Add(2 * time.Hour))
	refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(10 * time.Millisecond)
		writeJSON(w, 200, map[string]string{"access_token": newToken, "refresh_token": "<example-rotated-refresh>"})
	}))
	defer refresh.Close()
	t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE", refresh.URL)
	var wg sync.WaitGroup
	body, _ := json.Marshal(map[string]string{"access_token": rejected})
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := roleInternalRequest(a, "POST", "/internal/credentials/slot/rejected", a.role.secret, string(body))
			if w.Code != 200 {
				t.Errorf("rejection status %d", w.Code)
				return
			}
			var lease roleLease
			if err := json.Unmarshal(w.Body.Bytes(), &lease); err != nil || lease.AccessToken != newToken {
				t.Error("did not return current credential")
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("CAS refreshed %d times", calls.Load())
	}
}

func TestConnectionSnapshotPersistenceAndCleanup(t *testing.T) {
	control, _ := roleFixture(t)
	server := httptest.NewServer(control.controlInternalMux())
	defer server.Close()
	a := testApp(t, nil)
	testRole(t, a, "connection", server.URL)
	// Even an existing local config is never read, migrated or rewritten.
	configPath := filepath.Join(a.dataDir, "config.json")
	if err := os.WriteFile(configPath, []byte("do not read or overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.load(); err != nil {
		t.Fatal(err)
	}
	item := a.config.Accounts[0]
	if !a.usableLocked(item, "gpt-test", time.Now()) || !a.hasUsableAuthLocked(item) {
		t.Fatal("snapshot account unusable without auth.json")
	}
	if auth, err := a.codexAuth(item); err != nil || auth.AccessToken != "" || auth.AccountID != "workspace-test" {
		t.Fatal("routing auth must be snapshot metadata before lease")
	}
	if _, err := a.refreshedCodexAuth(item); err != nil {
		t.Fatal(err)
	}
	if a.upstreamIdentityKeyLocked(item) != control.upstreamIdentityKeyLocked(control.config.Accounts[0]) {
		t.Fatal("identity differs from control")
	}
	now := time.Now()
	seedDeviceAuthIdentityHistory(a, item.ID, now)
	control.config.Accounts[0].PendingAuthVerification = true
	if err := a.pollControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(a.state.StickySessions) == 0 || len(a.role.leases) != 0 {
		t.Fatal("pending repair must retain affinity but revoke lease")
	}
	if _, err := a.refreshedCodexAuth(item); err == nil {
		t.Fatal("pending snapshot allowed a lease")
	}
	control.config.Accounts[0].PendingAuthVerification = false
	control.config.Accounts[0].Enabled = false
	if err := a.pollControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(a.state.StickySessions) != 0 || len(a.role.leases) != 0 {
		t.Fatal("disable did not clear routes/leases")
	}
	if _, err := a.refreshedCodexAuth(item); err == nil {
		t.Fatal("stale selected account bypassed disabled snapshot")
	}
	control.config.Accounts[0].Enabled = true
	if err := a.pollControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	seedDeviceAuthIdentityHistory(a, item.ID, now)
	control.config.Accounts[0].AccountID = "replacement-test"
	if err := a.pollControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(a.state.PromptCache) != 0 || len(a.state.StickySessions) != 0 || a.connectionAccountCurrent(item) {
		t.Fatal("identity replacement retained history/socket")
	}
	for _, path := range []string{"/admin/api/login", "/admin/api/settings", "/admin/api/public-dashboard/accounts/x/repair", "/admin/api/accounts/slot/quota/reset-credit"} {
		if w := roleRequest(a, "POST", path, a.role.secret, "{}"); w.Code != 404 && w.Code != 405 {
			t.Fatalf("mutation exposed: %s %d", path, w.Code)
		}
	}
	if roleRequest(a, "GET", "/internal/state", a.role.secret, "").Code != 404 {
		t.Fatal("connection exposed control API")
	}
	data, err := os.ReadFile(configPath)
	if err != nil || string(data) != "do not read or overwrite" {
		t.Fatal("connection wrote config")
	}
	if _, err := os.Stat(filepath.Join(a.dataDir, "accounts")); !os.IsNotExist(err) {
		t.Fatal("connection created credential files")
	}
	if _, err := os.Stat(filepath.Join(a.dataDir, "state", "runtime.json")); err != nil {
		t.Fatal(err)
	}
}

func TestConnectionOfflineBoundsAndLeaseCache(t *testing.T) {
	control, token := roleFixture(t)
	var leases atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/internal/credentials/") {
			leases.Add(1)
		}
		control.controlInternalMux().ServeHTTP(w, r)
	}))
	defer server.Close()
	a := testApp(t, nil)
	testRole(t, a, "connection", server.URL)
	if err := a.load(); err != nil {
		t.Fatal(err)
	}
	item := a.config.Accounts[0]
	for i := 0; i < 3; i++ {
		auth, err := a.refreshedCodexAuth(item)
		if err != nil || auth.AccessToken != token {
			t.Fatal("lease failed", err)
		}
	}
	if leases.Load() != 1 {
		t.Fatal("lease was not cached")
	}
	server.Close()
	if a.pollControl(context.Background()) == nil {
		t.Fatal("expected offline poll error")
	}
	if _, err := a.refreshedCodexAuth(item); err != nil {
		t.Fatal("offline lease lost", err)
	}
	now := time.Now()
	if _, err := a.connectionAuth(item.ID, now.Add(31*time.Minute)); err == nil {
		t.Fatal("offline maximum ignored")
	}
	lease := a.role.leases[item.ID]
	lease.ExpiresAt = now.Add(time.Minute)
	a.role.leases[item.ID] = lease
	if _, err := a.connectionAuth(item.ID, now.Add(2*time.Minute)); err == nil {
		t.Fatal("token expiry ignored")
	}
	if _, err := a.refreshedCodexAuth(item); err != nil {
		t.Fatal("refresh-margin outage should retain unexpired lease", err)
	}
	a.role.lastState = now.Add(-31 * time.Minute)
	if _, err := a.refreshedCodexAuth(item); err == nil {
		t.Fatal("lease bypassed offline maximum")
	}
	a.role.lastState = now
	if _, err := a.refreshCodexAuthAfterRejection(context.Background(), item, token); err == nil {
		t.Fatal("offline rejected token reused")
	}
	if _, err := a.refreshedCodexAuth(item); err == nil {
		t.Fatal("rejected cache retained")
	}
}

func TestConnectionHTTPAndWSRejection(t *testing.T) {
	for _, protocol := range []string{"http", "ws", "native-models", "native-responses", "native-compact"} {
		t.Run(protocol, func(t *testing.T) {
			control, _ := roleFixture(t)
			broker := httptest.NewServer(control.controlInternalMux())
			defer broker.Close()
			var refreshes, attempts atomic.Int32
			fresh := fakeJWT(time.Now().Add(2 * time.Hour))
			refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				refreshes.Add(1)
				writeJSON(w, 200, map[string]string{"access_token": fresh, "refresh_token": "<example-rotated>"})
			}))
			defer refresh.Close()
			t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE", refresh.URL)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				if r.Header.Get("Authorization") != "Bearer "+fresh {
					w.WriteHeader(401)
					return
				}
				if r.Header.Get("ChatGPT-Account-ID") != "workspace-test" {
					t.Error("lost leased identity")
				}
				if protocol == "ws" {
					conn, err := acceptWebSocket(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					_, _, _ = conn.ReadMessage()
				} else {
					writeJSON(w, 200, map[string]any{"id": "resp_role", "object": "response", "output": []any{}})
				}
			}))
			defer upstream.Close()
			a := testApp(t, nil)
			testRole(t, a, "connection", broker.URL)
			if err := a.load(); err != nil {
				t.Fatal(err)
			}
			a.codexBaseURL = upstream.URL + "/backend-api"
			if protocol == "ws" {
				a.codexTransportURL = upstream.URL
				conn, _, refusal, err := a.dialCodexWebSocket(context.Background(), a.config.Accounts[0], http.Header{}, "gpt-test")
				if err != nil || conn == nil {
					t.Fatalf("WS failed: %v, refusal %v", err, refusal)
				}
				conn.Close()
			} else {
				method, path := "POST", "/v1/responses"
				switch protocol {
				case "native-models":
					method, path = "GET", "/k/client-key/backend-api/codex/models"
				case "native-responses":
					path = "/k/client-key/backend-api/codex/responses"
				case "native-compact":
					path = "/k/client-key/backend-api/codex/responses/compact"
				}
				r := httptest.NewRequest(method, path, strings.NewReader(`{"model":"gpt-test","input":"hello"}`))
				r.Header.Set("Authorization", "Bearer client-key")
				w := httptest.NewRecorder()
				a.mux().ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatalf("HTTP status %d: %s", w.Code, w.Body.String())
				}
			}
			if attempts.Load() != 2 || refreshes.Load() != 1 {
				t.Fatalf("attempts=%d refreshes=%d", attempts.Load(), refreshes.Load())
			}
			if _, err := os.Stat(filepath.Join(a.dataDir, "config.json")); !os.IsNotExist(err) {
				t.Fatal("request wrote config")
			}
			if _, err := os.Stat(filepath.Join(a.dataDir, "accounts")); !os.IsNotExist(err) {
				t.Fatal("request wrote auth")
			}
		})
	}
}

// Control-only entry points must refuse even when a stale credential file is
// present locally. No CLI, upstream maintenance request, or config save may
// turn a connection node into a second credential writer.
func TestConnectionControlOnlyBoundaries(t *testing.T) {
	a, _ := roleFixture(t)
	testRole(t, a, "connection", "http://control.invalid")
	item := a.config.Accounts[0]
	path := filepath.Join(a.accountCodexHome(item.ID), "auth.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.readCodexAuthFile(item); err == nil {
		t.Fatal("connection read local auth.json")
	}
	if err := a.refreshCodexAuthContext(context.Background(), item, true); err == nil {
		t.Fatal("connection performed local refresh")
	}
	a.codexPrimeRunner = func(context.Context, string, string, string) error {
		t.Fatal("connection ran Codex CLI prime")
		return nil
	}
	if err := a.primeWithCodexCLI(context.Background(), item, "gpt-test"); err == nil {
		t.Fatal("connection allowed quota prime")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("connection sent maintenance request")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	for _, transport := range []string{"", upstream.URL} {
		a.codexTransportURL = transport
		if _, err := a.sendCodexOwnRequest(context.Background(), item.ID, "GET", upstream.URL, "", nil, nil); err == nil {
			t.Fatal("connection allowed maintenance request")
		}
	}
	a.mu.Lock()
	_, loginErr := a.startLoginJobLocked(item)
	_, repairErr := a.startPublicRepairJobLocked(item)
	saveErr := a.saveCompletedAuthVerificationLocked()
	a.mu.Unlock()
	if loginErr == nil || repairErr == nil || len(a.jobs) != 0 {
		t.Fatal("connection started a login/repair job")
	}
	if saveErr != nil {
		t.Fatal(saveErr)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("connection changed local auth.json")
	}
	if _, err := os.Stat(filepath.Join(a.dataDir, "config.json")); !os.IsNotExist(err) {
		t.Fatal("auth verification wrote connection config.json")
	}
}

// The handshake leases before a client sends anything. A same-slot identity
// replacement must redial before the first send, or close an already-used
// socket so Codex can resend its incremental request in full.
func TestConnectionWebSocketIdentityReplacement(t *testing.T) {
	for _, sent := range []bool{false, true} {
		t.Run(fmt.Sprintf("sent=%v", sent), func(t *testing.T) {
			control, token := roleFixture(t)
			broker := httptest.NewServer(control.controlInternalMux())
			defer broker.Close()
			a := testApp(t, nil)
			testRole(t, a, "connection", broker.URL)
			if err := a.load(); err != nil {
				t.Fatal(err)
			}
			upstream := &fakeCodexUpstream{
				handshake: http.Header{"X-Codex-Turn-State": {"ts-old-identity"}},
				script:    func(string) []string { return completedEvents("resp_role_identity") },
			}
			transport := httptest.NewServer(upstream)
			defer transport.Close()
			a.codexTransportURL = transport.URL
			pool := httptest.NewServer(a.mux())
			defer pool.Close()
			conn := dialPool(t, "ws"+strings.TrimPrefix(pool.URL, "http")+"/k/client-key/backend-api/codex/responses")
			defer conn.Close()
			upstream.mu.Lock()
			leased := len(upstream.handshakes) == 1 && upstream.handshakes[0].Get("Authorization") == "Bearer "+token
			upstream.mu.Unlock()
			if !leased {
				t.Fatal("handshake did not acquire the control lease before the first request")
			}
			turn := `{"type":"response.create","model":"gpt-test","input":[],"client_metadata":{"x-codex-turn-state":"ts-old-identity"}}`
			if sent {
				if err := conn.WriteMessage(wsOpText, []byte(turn)); err != nil {
					t.Fatal(err)
				}
				if events := readUntilTerminal(t, conn); len(events) == 0 || !strings.Contains(events[len(events)-1], "response.completed") {
					t.Fatal("initial response did not complete")
				}
				waitInFlightZero(t, a)
			}
			control.mu.Lock()
			control.config.Accounts[0].AccountID = "replacement-test"
			control.mu.Unlock()
			writeCodexDeviceAuth(t, control, "slot", "replacement-test", "replacement@example.test")
			upstream.handshake = http.Header{"X-Codex-Turn-State": {"ts-new-identity"}}
			if err := a.pollControl(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := conn.WriteMessage(wsOpText, []byte(turn)); err != nil {
				t.Fatal(err)
			}
			events := readUntilTerminal(t, conn)
			waitInFlightZero(t, a)
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			if sent {
				if len(events) != 0 || len(upstream.handshakes) != 1 || len(upstream.messages) != 1 {
					t.Fatal("used socket did not close before sending into the old identity")
				}
			} else if len(events) == 0 || !strings.Contains(events[len(events)-1], "response.completed") || len(upstream.handshakes) != 2 || len(upstream.messages) != 1 || upstream.handshakes[1].Get("ChatGPT-Account-ID") != "replacement-test" || strings.Contains(upstream.messages[0], "ts-old-identity") {
				t.Fatal("first request did not redial with the replacement lease")
			}
		})
	}
}

func TestConnectionRestartAndQuotaQuarantine(t *testing.T) {
	control, _ := roleFixture(t)
	broker := httptest.NewServer(control.controlInternalMux())
	defer broker.Close()
	a := testApp(t, nil)
	testRole(t, a, "connection", broker.URL)
	if err := a.load(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedDeviceAuthIdentityHistory(a, "slot", now)
	if err := a.saveLocked(); err != nil {
		t.Fatal(err)
	}
	if err := a.pollControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !quotaErrorBlocksRouting(a.state.Quotas["slot"].QuotaError) {
		t.Fatal("poll erased local auth quarantine")
	}
	control.state.Quotas["slot"] = quotaSnapshot{AccountID: "slot", LastSuccessfulRefreshAt: now.Add(time.Second)}
	if err := a.pollControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	if quotaErrorBlocksRouting(a.state.Quotas["slot"].QuotaError) {
		t.Fatal("fresh successful quota did not recover quarantine")
	}
	seedDeviceAuthIdentityHistory(a, "slot", now)
	if err := a.saveLocked(); err != nil {
		t.Fatal(err)
	}
	restarted := testApp(t, nil)
	restarted.dataDir = a.dataDir
	testRole(t, restarted, "connection", broker.URL)
	if err := restarted.load(); err != nil {
		t.Fatal(err)
	}
	if len(restarted.state.StickySessions) == 0 || len(restarted.role.leases) != 0 {
		t.Fatal("restart lost runtime or restored credentials")
	}
	control.config.Accounts[0].AccountID = "changed-while-stopped"
	testRole(t, restarted, "connection", broker.URL)
	if err := restarted.load(); err != nil {
		t.Fatal(err)
	}
	if len(restarted.state.StickySessions) != 0 || len(restarted.state.PromptCache) != 0 {
		t.Fatal("restart inherited changed identity")
	}
	seedDeviceAuthIdentityHistory(restarted, "slot", now)
	control.config.Accounts = nil
	if err := restarted.pollControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(restarted.state.PromptCache) != 0 || len(restarted.state.ResponseBindings) != 0 {
		t.Fatal("removed account retained identity state")
	}
}
