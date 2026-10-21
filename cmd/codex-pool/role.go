package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const internalSecretHeader = "X-Codex-Pool-Internal-Secret"

const roleInternalAddressDefault = "0.0.0.0:8318"

func constantTimeSecretEqual(got, want string) bool {
	gotHash := sha256.Sum256([]byte(got))
	wantHash := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) == 1
}

// A nil role keeps every monolith path, including its mux and persistence,
// unchanged. Role state never contains a refresh token or a credential path.
type poolRole struct {
	name            string
	secret          string
	controlURL      string
	internalAddress string
	offlineMax      time.Duration
	client          *http.Client

	// Lock order is app.mu then mu. Credential reads under the routing lock
	// must remain memory-only; never perform a broker RPC from codexAuth.
	mu          sync.RWMutex
	accounts    map[string]account
	credentials map[string]roleCredentialState
	leases      map[string]roleLease
	identities  map[string]string
	lastState   time.Time
	online      bool

	// Request statistics are protected by app.mu, like the local state they
	// snapshot. Pending throughput stays memory-only, just like its chart.
	reported          roleStatsBaseline
	pendingThroughput []*throughputMeasurement
}

func (a *app) isConnection() bool { return a.role != nil && a.role.name == "connection" }

func (a *app) configureRole() error {
	name := os.Getenv("CODEX_POOL_ROLE")
	if name == "" {
		return nil
	}
	if name != "control" && name != "connection" {
		return errors.New("CODEX_POOL_ROLE must be empty, control, or connection")
	}
	secret := os.Getenv("CODEX_POOL_INTERNAL_SECRET")
	if strings.TrimSpace(secret) == "" {
		return errors.New("CODEX_POOL_INTERNAL_SECRET is required for roles")
	}
	for _, key := range a.apiKeys {
		if subtle.ConstantTimeCompare(key, []byte(secret)) == 1 {
			return errors.New("internal secret must differ from public API keys")
		}
	}
	r := &poolRole{name: name, secret: secret, offlineMax: 30 * time.Minute,
		client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			// Never forward the broker secret to a redirect destination.
			return http.ErrUseLastResponse
		}},
		accounts: map[string]account{}, credentials: map[string]roleCredentialState{},
		leases: map[string]roleLease{}, identities: map[string]string{}}
	if name == "connection" {
		if raw := os.Getenv("CODEX_POOL_CONTROL_OFFLINE_MAX"); raw != "" {
			duration, err := time.ParseDuration(raw)
			if err != nil || duration <= 0 {
				return errors.New("CODEX_POOL_CONTROL_OFFLINE_MAX must be a positive Go duration")
			}
			r.offlineMax = duration
		}
		u, err := url.Parse(os.Getenv("CODEX_POOL_CONTROL_URL"))
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return errors.New("CODEX_POOL_CONTROL_URL must be an http(s) origin without credentials")
		}
		r.controlURL = strings.TrimRight(u.String(), "/")
	}
	a.role = r
	if name == "control" {
		r.internalAddress = envOr("CODEX_POOL_INTERNAL_ADDR", roleInternalAddressDefault)
	}
	return nil
}

func (a *app) roleHandler(next http.Handler) http.Handler {
	if a.role == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Preserve the existing loopback-only drain probe. Its own handler
		// enforces reachability; the broker secret must not bypass that check.
		if r.URL.Path == "/internal/inflight" {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			// State and credential brokering are bound to the private control
			// listener below. Returning 404 here keeps the public listener from
			// becoming a credential endpoint even when the secret is known.
			http.NotFound(w, r)
			return
		}
		if a.isConnection() {
			// Keep status assets readable, but block all admin mutations before
			// dispatch (including public repair and login), not just owner APIs.
			if strings.HasPrefix(r.URL.Path, "/admin/api/") && r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.NotFound(w, r)
				return
			}
			if r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				a.role.mu.RLock()
				ready := time.Now().Before(a.role.lastState.Add(a.role.offlineMax))
				a.role.mu.RUnlock()
				if !ready {
					http.Error(w, "control state unavailable", http.StatusServiceUnavailable)
					return
				}
				a.handleHealthz(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
