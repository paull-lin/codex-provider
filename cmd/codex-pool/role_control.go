package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Keep the credential broker off the published UI/API listener. Both listeners
// stop accepting on cancellation; a failed broker also stops the public server,
// so control cannot remain apparently healthy without its internal service.
func (a *app) serveControlUntilStopped(ctx context.Context, public net.Listener) error {
	internal, err := net.Listen("tcp", a.role.internalAddress)
	if err != nil {
		_ = public.Close()
		return fmt.Errorf("listen control internal service: %w", err)
	}
	defer internal.Close()
	a.logger.Printf("control internal API listening on %s", internal.Addr())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{Handler: recovery(a.controlInternalMux()), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() {
		served <- server.Serve(internal)
		cancel()
	}()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		grace := a.shutdownGrace
		if grace <= 0 {
			grace = defaultShutdownGrace
		}
		drainCtx, stop := context.WithTimeout(context.Background(), grace)
		defer stop()
		// Use the same grace as the main listener, then close remaining
		// broker connections so none survive the control shutdown.
		if err := server.Shutdown(drainCtx); err != nil {
			_ = server.Close()
		}
	}()
	err = a.serveUntilStopped(ctx, public, a.mux())
	cancel()
	<-stopped
	internalErr := <-served
	if errors.Is(internalErr, http.ErrServerClosed) {
		internalErr = nil
	}
	return errors.Join(err, internalErr)
}

func (a *app) controlInternalMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/state", a.handleRoleState)
	mux.HandleFunc("POST /internal/credentials/{account}", a.handleRoleCredentials)
	mux.HandleFunc("POST /internal/credentials/{account}/rejected", a.handleRoleCredentials)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This secret grants credentials, never API/admin access. Empty
		// configuration must fail closed even in manually constructed apps.
		if a.role == nil || a.role.name != "control" || a.role.secret == "" ||
			!constantTimeSecretEqual(r.Header.Get(internalSecretHeader), a.role.secret) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

type roleCredentialState struct {
	Usable    bool      `json:"usable"`
	ExpiresAt time.Time `json:"expires_at"`
}

type roleSnapshot struct {
	Version     string                         `json:"version"`
	Config      config                         `json:"config"`
	Credentials map[string]roleCredentialState `json:"credentials"`
	Quotas      map[string]quotaSnapshot       `json:"quotas"`
}

type roleLease struct {
	AccessToken string    `json:"access_token"`
	AccountID   string    `json:"chatgpt_account_id"`
	FedRAMP     bool      `json:"fedramp"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func (l roleLease) auth() codexAuthInfo {
	return codexAuthInfo{AccessToken: l.AccessToken, AccountID: l.AccountID, FedRAMP: l.FedRAMP}
}

func (a *app) controlSnapshot() roleSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	snapshot := roleSnapshot{Config: a.config, Credentials: map[string]roleCredentialState{}, Quotas: a.state.Quotas}
	snapshot.Config.Accounts = append([]account(nil), a.config.Accounts...)
	for i := range snapshot.Config.Accounts {
		item := &snapshot.Config.Accounts[i]
		// A state poll never distributes credentials, including provider keys.
		// The MVP broker leases only Codex device-auth credentials; other
		// provider slots remain visible but cannot route on a connection node.
		item.CodexHome, item.UpstreamAPIKey = "", ""
		item.PendingAuthVerification = a.accountAuthVerificationPendingLocked(*item)
		credential := roleCredentialState{}
		if isCodexDeviceAuth(*item) {
			if auth, err := a.codexAuth(*item); err == nil {
				expiry, known := jwtExpiry(auth.AccessToken)
				credential = roleCredentialState{Usable: known && item.Enabled && !item.PendingPoolActivation && !item.PendingAuthVerification, ExpiresAt: expiry}
				// Match the monolith's identity fallback without asking connection
				// routing to read auth.json or acquire a credential lease.
				if item.AccountID == "" {
					item.AccountID = auth.AccountID
				}
				if item.Email == "" {
					item.Email = auth.Email
				}
				if item.OrganizationName == "" {
					item.OrganizationName = auth.OrganizationName
				}
			}
		}
		if !isCodexDeviceAuth(*item) {
			item.Enabled = false
			item.InPool = false
		}
		snapshot.Credentials[item.ID] = credential
	}
	// Content addressing avoids extra writes and catches quota/credential-file
	// changes as well as config mutations. Clone while locked: encoding later
	// must not race quota updates or share maps with a connection test fixture.
	data, _ := json.Marshal(snapshot)
	var detached roleSnapshot
	_ = json.Unmarshal(data, &detached)
	digest := sha256.Sum256(data)
	detached.Version = hex.EncodeToString(digest[:])
	return detached
}

func (a *app) handleRoleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.controlSnapshot())
}

func (a *app) roleCredentialAccount(id string) (account, int) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	item := a.accountLocked(id)
	if item == nil || !isCodexDeviceAuth(*item) {
		return account{}, http.StatusNotFound
	}
	if !item.Enabled || item.PendingPoolActivation || a.accountAuthVerificationPendingLocked(*item) {
		return account{}, http.StatusConflict
	}
	return *item, http.StatusOK
}

func (a *app) handleRoleCredentials(w http.ResponseWriter, r *http.Request) {
	item, status := a.roleCredentialAccount(r.PathValue("account"))
	if status != http.StatusOK {
		http.Error(w, "credential unavailable", status)
		return
	}
	var auth codexAuthInfo
	var err error
	if strings.HasSuffix(r.URL.Path, "/rejected") {
		var rejected struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&rejected); err != nil || rejected.AccessToken == "" {
			http.Error(w, "rejected access_token required", http.StatusBadRequest)
			return
		}
		// Pass the rejected value through unchanged. The existing per-account
		// lock and CAS also deduplicate delayed 401s after the first refresh.
		auth, err = a.refreshCodexAuthAfterRejection(r.Context(), item, rejected.AccessToken)
	} else {
		auth, err = a.refreshedCodexAuthContext(r.Context(), item)
	}
	if err != nil {
		http.Error(w, "credential unavailable", http.StatusBadGateway)
		return
	}
	// Recheck after I/O so a disable/login transition cannot release a lease.
	current, status := a.roleCredentialAccount(item.ID)
	if status != http.StatusOK {
		http.Error(w, "credential unavailable", status)
		return
	}
	if current.AccountID != item.AccountID || current.LastLoginAt != item.LastLoginAt {
		http.Error(w, "credential identity changed", http.StatusConflict)
		return
	}
	expiry, known := jwtExpiry(auth.AccessToken)
	if !known || !expiry.After(time.Now()) {
		http.Error(w, "credential expiry unavailable", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, roleLease{AccessToken: auth.AccessToken, AccountID: auth.AccountID, FedRAMP: auth.FedRAMP, ExpiresAt: expiry})
}
