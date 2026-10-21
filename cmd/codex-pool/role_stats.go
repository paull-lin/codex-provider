package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"time"
)

const roleThroughputPendingLimit = 4096
const roleStatsBodyLimit = 8 << 20

type roleStatsCounters struct {
	RequestCount                uint64 `json:"requestCount"`
	SuccessCount                uint64 `json:"successCount"`
	FailureCount                uint64 `json:"failureCount"`
	UpstreamResponseFailedCount uint64 `json:"upstreamResponseFailedCount"`
	StreamIncompleteCount       uint64 `json:"streamIncompleteCount"`
}

type roleStatsBaseline struct {
	roleStatsCounters
	PromptCache        map[string]promptCacheStat `json:"promptCache,omitempty"`
	Health             map[string]accountHealth   `json:"health,omitempty"`
	LastRoutingEventAt time.Time                  `json:"lastRoutingEventAt,omitempty"`
}

type roleStatsReport struct {
	roleStatsCounters
	PromptCache        map[string]promptCacheStat `json:"promptCache,omitempty"`
	Health             map[string]accountHealth   `json:"health,omitempty"`
	RoutingCacheEvents []routingCacheEvent        `json:"routingCacheEvents,omitempty"`
	Throughput         []*throughputMeasurement   `json:"throughput,omitempty"`
}

// Walk exported uint64 fields rather than enumerating cache counters. Future
// upstream counters must survive both subtraction and addition automatically;
// identity/timestamps are handled separately and never treated as counters.
func roleUint64Fields(dst, value any, add bool) bool {
	d, v := reflect.ValueOf(dst).Elem(), reflect.ValueOf(value)
	nonzero := false
	for i := 0; i < d.NumField(); i++ {
		if d.Type().Field(i).PkgPath != "" || d.Field(i).Kind() != reflect.Uint64 {
			continue
		}
		n := subSat(d.Field(i).Uint(), v.Field(i).Uint())
		if add {
			n = d.Field(i).Uint() + v.Field(i).Uint()
		}
		d.Field(i).SetUint(n)
		nonzero = nonzero || n != 0
	}
	return nonzero
}

func (a *app) connectionStatsLocked() (roleStatsReport, roleStatsBaseline, bool) {
	base := a.role.reported
	next := roleStatsBaseline{
		roleStatsCounters:  roleStatsCounters{a.state.RequestCount, a.state.SuccessCount, a.state.FailureCount, a.state.UpstreamResponseFailedCount, a.state.StreamIncompleteCount},
		PromptCache:        make(map[string]promptCacheStat, len(a.state.PromptCache)),
		Health:             make(map[string]accountHealth, len(a.state.Health)),
		LastRoutingEventAt: base.LastRoutingEventAt,
	}
	report := roleStatsReport{roleStatsCounters: next.roleStatsCounters, PromptCache: map[string]promptCacheStat{}, Health: map[string]accountHealth{}}
	changed := roleUint64Fields(&report.roleStatsCounters, base.roleStatsCounters, false)
	for key, stat := range a.state.PromptCache {
		next.PromptCache[key] = stat
		if roleUint64Fields(&stat, base.PromptCache[key], false) {
			report.PromptCache[key] = stat
		}
	}
	for id, health := range a.state.Health {
		next.Health[id] = health
		if health != base.Health[id] {
			report.Health[id] = health
		}
	}
	for _, event := range a.state.RoutingCacheEvents {
		if event.Timestamp.After(base.LastRoutingEventAt) {
			report.RoutingCacheEvents = append(report.RoutingCacheEvents, event)
		}
		if event.Timestamp.After(next.LastRoutingEventAt) {
			next.LastRoutingEventAt = event.Timestamp
		}
	}
	report.Throughput = append([]*throughputMeasurement(nil), a.role.pendingThroughput...)
	return report, next, changed || len(report.PromptCache)+len(report.Health)+len(report.RoutingCacheEvents)+len(report.Throughput) > 0
}

func (a *app) reportConnectionStats(ctx context.Context) error {
	// Only the startup/5s poller reports. Do not hold the routing lock over I/O;
	// acknowledge the captured totals, not traffic completed during the POST.
	a.mu.Lock()
	report, next, changed := a.connectionStatsLocked()
	a.mu.Unlock()
	if !changed {
		return nil
	}
	body, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if _, err := a.controlRequest(ctx, http.MethodPost, "/internal/stats", body, nil); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.role.reported = next
	// The bounded queue may have dropped old entries and gained new ones during
	// I/O. Remove only captured pointers, never a count of the current prefix.
	sent := make(map[*throughputMeasurement]bool, len(report.Throughput))
	for _, measurement := range report.Throughput {
		sent[measurement] = true
	}
	pending := a.role.pendingThroughput[:0]
	for _, measurement := range a.role.pendingThroughput {
		if !sent[measurement] {
			pending = append(pending, measurement)
		}
	}
	clear(a.role.pendingThroughput[len(pending):])
	a.role.pendingThroughput = pending
	// Runtime totals and the acknowledged baseline share one atomic file. A
	// crash/timeout before this save may duplicate a report: at-least-once only.
	return a.saveConnectionLocked()
}

func (a *app) queueRoleThroughputLocked(measurement throughputMeasurement) {
	if len(a.role.pendingThroughput) == roleThroughputPendingLimit {
		copy(a.role.pendingThroughput, a.role.pendingThroughput[1:])
		a.role.pendingThroughput = a.role.pendingThroughput[:roleThroughputPendingLimit-1]
	}
	a.role.pendingThroughput = append(a.role.pendingThroughput, &measurement)
}

func (a *app) clearReportedAccountStatsLocked(id string) {
	// Identity cleanup resets local per-account totals. Its old baseline must
	// not suppress the replacement identity's first requests in the same slot.
	deletePromptCacheForAccount(a.role.reported.PromptCache, id)
	delete(a.role.reported.Health, id)
	pending := a.role.pendingThroughput[:0]
	for _, measurement := range a.role.pendingThroughput {
		if measurement.AccountID != id {
			pending = append(pending, measurement)
		}
	}
	clear(a.role.pendingThroughput[len(pending):])
	a.role.pendingThroughput = pending
}

func (a *app) handleRoleStats(w http.ResponseWriter, r *http.Request) {
	var report roleStatsReport
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, roleStatsBodyLimit))
	if err := decoder.Decode(&report); err != nil {
		http.Error(w, "invalid statistics", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "invalid statistics", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mergeRoleStatsLocked(report, time.Now().UTC())
	if err := a.saveControlStatsLocked(); err != nil {
		http.Error(w, "statistics save failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) mergeRoleStatsLocked(report roleStatsReport, now time.Time) {
	a.state.RequestCount += report.RequestCount
	a.state.SuccessCount += report.SuccessCount
	a.state.FailureCount += report.FailureCount
	a.state.UpstreamResponseFailedCount += report.UpstreamResponseFailedCount
	a.state.StreamIncompleteCount += report.StreamIncompleteCount
	known := make(map[string]bool, len(a.config.Accounts))
	for _, item := range a.config.Accounts {
		known[item.ID] = true
	}
	if a.state.PromptCache == nil {
		a.state.PromptCache = map[string]promptCacheStat{}
	}
	if a.state.Health == nil {
		a.state.Health = map[string]accountHealth{}
	}
	// A delayed reporter cannot resurrect a deleted account. Window baselines
	// remain entirely control-owned: a reset must not reset connection totals.
	for key, delta := range report.PromptCache {
		if !known[delta.AccountID] {
			continue
		}
		stat := a.state.PromptCache[key]
		stat.AccountID, stat.ModelID, stat.AgentKind = delta.AccountID, delta.ModelID, delta.AgentKind
		roleUint64Fields(&stat, delta, true)
		if delta.UpdatedAt.After(stat.UpdatedAt) {
			stat.UpdatedAt = delta.UpdatedAt
		}
		a.state.PromptCache[key] = stat
	}
	for id, incoming := range report.Health {
		if !known[id] {
			continue
		}
		current := a.state.Health[id]
		if incoming.LastFailureAt.After(current.LastFailureAt) {
			current.LastFailureAt, current.LastFailureReason = incoming.LastFailureAt, incoming.LastFailureReason
			current.ConsecutiveFailure = incoming.ConsecutiveFailure
		} else if incoming.LastFailureAt.Equal(current.LastFailureAt) && incoming.LastSuccessAt.After(current.LastSuccessAt) {
			// The same newest failure's reporter may subsequently recover.
			// Accept its reset without replacing the stored failure reason.
			current.ConsecutiveFailure = incoming.ConsecutiveFailure
		}
		if incoming.LastSuccessAt.After(current.LastSuccessAt) {
			current.LastSuccessAt = incoming.LastSuccessAt
		}
		a.state.Health[id] = current
	}
	for _, event := range report.RoutingCacheEvents {
		if known[event.AccountID] {
			a.state.RoutingCacheEvents = append(a.state.RoutingCacheEvents, event)
		}
	}
	// Reports can arrive in opposite order. Upstream pruning and dashboard
	// reversal assume chronological storage; reuse their TTL and cap unchanged.
	sort.SliceStable(a.state.RoutingCacheEvents, func(i, j int) bool {
		return a.state.RoutingCacheEvents[i].Timestamp.Before(a.state.RoutingCacheEvents[j].Timestamp)
	})
	a.pruneRoutingCacheEventsLocked(now)
	for _, measurement := range report.Throughput {
		if measurement != nil && known[measurement.AccountID] {
			a.recordThroughputMeasurementLocked(*measurement)
		}
	}
	a.pruneThroughputBucketsLocked(now)
}

// Statistics are runtime-only. saveLocked would also rewrite config.json, the
// account authority, on every report cycle and stamp a new config UpdatedAt.
func (a *app) saveControlStatsLocked() error {
	a.state.UpdatedAt = time.Now().UTC()
	return writeJSONAtomic(filepath.Join(a.dataDir, "state", "runtime.json"), a.state)
}
