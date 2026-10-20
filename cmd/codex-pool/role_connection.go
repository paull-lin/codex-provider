package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Only runtime and identity markers survive restart. Markers let the first
// fresh snapshot purge bindings when a slot changed identity while we were
// down; neither credentials nor authoritative config belong on this volume.
type connectionRuntime struct {
	state
	Identities map[string]string `json:"controlIdentities"`
}

func (a *app) loadConnection() error {
	if err := os.MkdirAll(filepath.Join(a.dataDir, "state"), 0o700); err != nil {
		return err
	}
	runtime := connectionRuntime{state: state{
		StickySessions: map[string]stickySession{}, ResponseBindings: map[string]responseBinding{},
		ThreadBindings: map[string]threadBinding{}, LineageBindings: map[string]lineageBinding{},
		Cooldowns: map[string][]cooldown{}, Health: map[string]accountHealth{},
		Quotas: map[string]quotaSnapshot{}, PromptCache: map[string]promptCacheStat{},
	}, Identities: map[string]string{}}
	if err := readJSON(filepath.Join(a.dataDir, "state", "runtime.json"), &runtime); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	a.state = runtime.state
	a.state.LegacyThroughputBuckets = nil
	a.role.identities = runtime.Identities
	// A restart cannot serve from stale persisted authority. Obtain an initial
	// snapshot before listening; leases intentionally start empty.
	return a.pollControl(context.Background())
}

func (a *app) saveConnectionLocked() error {
	a.state.UpdatedAt = time.Now().UTC()
	a.role.mu.RLock()
	defer a.role.mu.RUnlock()
	return writeJSONAtomic(filepath.Join(a.dataDir, "state", "runtime.json"), connectionRuntime{state: a.state, Identities: a.role.identities})
}

func (a *app) startControlPoller(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := a.pollControl(ctx); err != nil {
					// Never log response bodies, URLs with query data, or tokens.
					a.logger.Print("control state poll unavailable; cached leases remain bounded by expiry and offline limit")
				}
			}
		}
	}()
}

func (a *app) controlRequest(ctx context.Context, method, path string, body []byte, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.role.controlURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("invalid control request")
	}
	req.Header.Set(internalSecretHeader, a.role.secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := a.role.client.Do(req)
	if err != nil {
		return 0, errors.New("control unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, fmt.Errorf("control returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(out); err != nil {
		return response.StatusCode, errors.New("invalid control response")
	}
	return response.StatusCode, nil
}

func (a *app) pollControl(ctx context.Context) error {
	var snapshot roleSnapshot
	_, err := a.controlRequest(ctx, http.MethodGet, "/internal/state", nil, &snapshot)
	if err == nil && snapshot.Version == "" {
		err = errors.New("missing control snapshot version")
	}
	if err != nil {
		a.role.mu.Lock()
		a.role.online = false
		a.role.mu.Unlock()
		return err
	}
	return a.applyControlSnapshot(snapshot, time.Now().UTC())
}

func roleIdentity(item account) string {
	organization := ""
	if item.AccountID == "" {
		organization = item.OrganizationName
	}
	data, _ := json.Marshal([]string{item.AuthType, item.AccountID, normalizeEmail(item.Email), organization})
	return string(data)
}

func (a *app) connectionAccountCurrent(bound account) bool {
	if !a.isConnection() {
		return true
	}
	a.role.mu.RLock()
	defer a.role.mu.RUnlock()
	// A slot ID survives sign-in replacement; a WS upstream socket does not.
	// Reconnect on the next turn instead of sending new identity's conversation
	// into the old socket, even when selection returns the same local slot.
	return a.role.identities[bound.ID] == roleIdentity(bound)
}

func roleAccountAvailable(item account, credential roleCredentialState) bool {
	return isCodexDeviceAuth(item) && item.Enabled && !item.PendingPoolActivation && !item.PendingAuthVerification && credential.Usable
}

func (a *app) applyControlSnapshot(snapshot roleSnapshot, now time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.role
	r.mu.Lock()
	accounts := make(map[string]account, len(snapshot.Config.Accounts))
	identities := make(map[string]string, len(snapshot.Config.Accounts))
	for _, item := range snapshot.Config.Accounts {
		accounts[item.ID] = item
		identities[item.ID] = roleIdentity(item)
	}
	for id, identity := range r.identities {
		if identities[id] != identity {
			a.clearAccountIdentityScopedStateLocked(id)
			// Upstream's new turn-state registry keys issuers by local slot ID.
			// Keep a non-slot tombstone: deleting an old issuer would fall back to
			// the newly rebound sticky slot and send old identity state to it.
			a.turnStateMu.Lock()
			for value, issuer := range a.turnStateIssuers {
				if issuer.accountID == id {
					issuer.accountID = "<replaced>"
					a.turnStateIssuers[value] = issuer
				}
			}
			a.turnStateMu.Unlock()
			delete(r.leases, id)
		}
	}
	for id, item := range accounts {
		// Match monolith disable/pool-remove cleanup; pending repair alone keeps
		// affinity, but revokes leases so it can never route during verification.
		if !item.Enabled || !item.InPool {
			a.clearStickyForAccountLocked(id)
		}
		if !roleAccountAvailable(item, snapshot.Credentials[id]) {
			delete(r.leases, id)
		}
		quota := snapshot.Quotas[id]
		prior := a.state.Quotas[id]
		// Fresh control telemetry must not erase a local 401 quarantine. Only a
		// later successful control quota read proves that credential recovered.
		if quotaErrorBlocksRouting(prior.QuotaError) && !quota.LastSuccessfulRefreshAt.After(prior.QuotaError.Timestamp) {
			quota.QuotaError = prior.QuotaError
		}
		a.state.Quotas[id] = quota
	}
	r.accounts, r.credentials, r.identities = accounts, snapshot.Credentials, identities
	r.lastState, r.online = now, true
	r.mu.Unlock()
	a.config = snapshot.Config
	if a.config.PreserveProQuota != nil {
		a.preserveProQuota = *a.config.PreserveProQuota
	}
	if a.config.LineageAffinity != nil {
		a.lineageAffinity = *a.config.LineageAffinity
	}
	a.pruneExpiredRuntimeStateLocked(now)
	return a.saveConnectionLocked()
}

func (a *app) connectionAuth(id string, now time.Time) (codexAuthInfo, error) {
	r := a.role
	r.mu.RLock()
	defer r.mu.RUnlock()
	item, exists := r.accounts[id]
	if !exists || !roleAccountAvailable(item, r.credentials[id]) || !now.Before(r.lastState.Add(r.offlineMax)) {
		return codexAuthInfo{}, errors.New("control credential unavailable")
	}
	lease := r.leases[id]
	if !r.online && (lease.AccessToken == "" || !now.Before(lease.ExpiresAt)) {
		return codexAuthInfo{}, errors.New("offline credential expired or missing")
	}
	// Metadata is enough for selection before the first lease. Never invent an
	// access token here: only refreshedCodexAuthContext may lease one over HTTP.
	auth := codexAuthInfo{AccountID: item.AccountID, Email: item.Email, OrganizationName: item.OrganizationName, PlanType: item.PlanType, PlanLimit: item.PlanLimit}
	if now.Before(lease.ExpiresAt) {
		auth.AccessToken, auth.FedRAMP = lease.AccessToken, lease.FedRAMP
	}
	return auth, nil
}

func (a *app) connectionLease(ctx context.Context, id, rejected string) (codexAuthInfo, error) {
	lock := a.codexAuthLock(id)
	lock.Lock()
	defer lock.Unlock()
	r := a.role
	if _, err := a.connectionAuth(id, time.Now()); err != nil {
		return codexAuthInfo{}, err
	}
	r.mu.RLock()
	lease := r.leases[id]
	identity := r.identities[id]
	r.mu.RUnlock()
	if rejected == "" && lease.AccessToken != "" && time.Now().Before(lease.ExpiresAt.Add(-codexTokenRefreshWindow)) {
		return lease.auth(), nil
	}
	path := "/internal/credentials/" + url.PathEscape(id)
	var body []byte
	if rejected != "" {
		path += "/rejected"
		body, _ = json.Marshal(map[string]string{"access_token": rejected})
		// A rejected token is never an offline fallback, even if its JWT has
		// time left. Control's CAS decides whether a newer token already exists.
		r.mu.Lock()
		delete(r.leases, id)
		r.mu.Unlock()
	}
	var current roleLease
	status, err := a.controlRequest(ctx, http.MethodPost, path, body, &current)
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	item, exists := r.accounts[id]
	if !exists || r.identities[id] != identity || !roleAccountAvailable(item, r.credentials[id]) || !now.Before(r.lastState.Add(r.offlineMax)) {
		return codexAuthInfo{}, errors.New("control account changed during lease")
	}
	if err != nil {
		if status == 0 || status >= 500 {
			r.online = false
			if rejected == "" && lease.AccessToken != "" && now.Before(lease.ExpiresAt) {
				return lease.auth(), nil
			}
		} else {
			delete(r.leases, id)
			// An explicit refusal is authority, not an outage. Wait for a new
			// snapshot before this account can be selected again.
			credential := r.credentials[id]
			credential.Usable = false
			r.credentials[id] = credential
		}
		return codexAuthInfo{}, err
	}
	if current.AccessToken == "" || !current.ExpiresAt.After(now) || (item.AccountID != "" && current.AccountID != item.AccountID) {
		return codexAuthInfo{}, errors.New("invalid control credential lease")
	}
	r.leases[id] = current
	return current.auth(), nil
}
