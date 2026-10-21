package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRoleStatsGenericCounters(t *testing.T) {
	// An upstream-added exported counter requires no change to the walker.
	type futureStat struct {
		AccountID     string
		FutureCounter uint64
		private       uint64
	}
	future := futureStat{"slot", 9, 7}
	if !roleUint64Fields(&future, futureStat{FutureCounter: 4, private: 6}, false) || future.FutureCounter != 5 || future.AccountID != "slot" || future.private != 7 {
		t.Fatalf("generic delta = %+v", future)
	}
	roleUint64Fields(&future, futureStat{FutureCounter: 4}, true)
	if future.FutureCounter != 9 {
		t.Fatal("generic addition lost new counter")
	}

	a := testApp(t, nil)
	testRole(t, a, "connection", "http://control.invalid")
	now := time.Now().UTC()
	stat := promptCacheStat{AccountID: "slot", ModelID: "gpt-test", AgentKind: "subagent", UpdatedAt: now}
	v := reflect.ValueOf(&stat).Elem()
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).Kind() == reflect.Uint64 {
			v.Field(i).SetUint(10)
		}
	}
	a.state.PromptCache["slot:gpt-test:subagent"] = stat
	a.state.RequestCount, a.state.SuccessCount, a.state.FailureCount = 10, 8, 2
	a.state.UpstreamResponseFailedCount, a.state.StreamIncompleteCount = 2, 1
	a.state.Health["slot"] = accountHealth{LastSuccessAt: now}
	a.state.RoutingCacheEvents = []routingCacheEvent{{AccountID: "slot", Timestamp: now}}
	first, baseline, changed := a.connectionStatsLocked()
	if !changed || !reflect.DeepEqual(first.PromptCache["slot:gpt-test:subagent"], stat) || first.RequestCount != 10 || first.StreamIncompleteCount != 1 || len(first.Health) != 1 || len(first.RoutingCacheEvents) != 1 {
		t.Fatalf("initial report = %+v", first)
	}
	a.role.reported = baseline
	if _, _, changed := a.connectionStatsLocked(); changed {
		t.Fatal("unchanged totals produced a report")
	}
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).Kind() == reflect.Uint64 {
			v.Field(i).SetUint(13)
		}
	}
	a.state.PromptCache["slot:gpt-test:subagent"] = stat
	a.state.RequestCount++
	a.state.RoutingCacheEvents = append(a.state.RoutingCacheEvents, routingCacheEvent{AccountID: "slot", Timestamp: now.Add(time.Second)})
	delta, _, _ := a.connectionStatsLocked()
	deltaStat := delta.PromptCache["slot:gpt-test:subagent"]
	d := reflect.ValueOf(deltaStat)
	for i := 0; i < d.NumField(); i++ {
		if d.Field(i).Kind() == reflect.Uint64 && d.Field(i).Uint() != 3 {
			t.Fatalf("missing delta for %s", d.Type().Field(i).Name)
		}
	}
	if deltaStat.AccountID != "slot" || deltaStat.ModelID != "gpt-test" || deltaStat.AgentKind != "subagent" || delta.RequestCount != 1 || len(delta.Health) != 0 || len(delta.RoutingCacheEvents) != 1 {
		t.Fatalf("incremental report = %+v", delta)
	}
	a.clearReportedAccountStatsLocked("slot")
	delete(a.state.PromptCache, "slot:gpt-test:subagent")
	a.state.PromptCache["slot:gpt-test:subagent"] = promptCacheStat{AccountID: "slot", RequestCount: 1}
	delta, _, _ = a.connectionStatsLocked()
	if delta.PromptCache["slot:gpt-test:subagent"].RequestCount != 1 {
		t.Fatal("replacement identity inherited old baseline")
	}
}

func TestRoleStatsReportingRetryRestartAndInflight(t *testing.T) {
	control, _ := roleFixture(t)
	connection := testApp(t, nil)
	posts, status := 0, http.StatusServiceUnavailable
	var reports []roleStatsReport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/stats" {
			control.controlInternalMux().ServeHTTP(w, r)
			return
		}
		if r.Header.Get(internalSecretHeader) != control.role.secret {
			t.Error("missing report secret")
		}
		var report roleStatsReport
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Error(err)
		}
		posts++
		reports = append(reports, report)
		if status == http.StatusNoContent {
			connection.mu.Lock()
			connection.state.RequestCount++
			connection.queueRoleThroughputLocked(throughputMeasurement{AccountID: "slot", StartedAt: time.Now(), CompletedAt: time.Now()})
			connection.mu.Unlock()
		}
		w.WriteHeader(status)
	}))
	defer server.Close()
	testRole(t, connection, "connection", server.URL)
	if err := connection.loadConnection(); err != nil {
		t.Fatal(err)
	}
	if posts != 0 {
		t.Fatal("empty baseline posted")
	}
	connection.state.RequestCount = 2
	connection.state.PromptCache["slot:gpt-test:main"] = promptCacheStat{AccountID: "slot", ModelID: "gpt-test", RequestCount: 2}
	connection.recordThroughputMeasurementLocked(throughputMeasurement{AccountID: "slot", StartedAt: time.Now(), CompletedAt: time.Now(), Success: true})
	if err := connection.reportConnectionStats(context.Background()); err == nil {
		t.Fatal("POST failure ignored")
	}
	if connection.role.reported.RequestCount != 0 || len(connection.role.pendingThroughput) != 1 {
		t.Fatal("failed report advanced baseline")
	}
	connection.state.RequestCount++
	status = http.StatusNoContent
	if err := connection.pollControl(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reports[1].RequestCount != 3 || connection.role.reported.RequestCount != 3 || connection.state.RequestCount != 4 || len(connection.role.pendingThroughput) != 1 {
		t.Fatal("retry/in-flight accounting lost traffic")
	}
	var saved connectionRuntime
	if err := readJSON(filepath.Join(connection.dataDir, "state", "runtime.json"), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Reported.RequestCount != 3 || saved.RequestCount != 4 || len(saved.Reported.PromptCache) != 1 {
		t.Fatalf("saved baseline = %+v", saved.Reported)
	}
	status = http.StatusServiceUnavailable
	restarted := testApp(t, nil)
	restarted.dataDir = connection.dataDir
	testRole(t, restarted, "connection", server.URL)
	if err := restarted.loadConnection(); err != nil {
		t.Fatal(err)
	}
	last := reports[len(reports)-1]
	if last.RequestCount != 1 || len(last.PromptCache) != 0 || len(last.Throughput) != 0 {
		t.Fatalf("restart resent lifetime totals: %+v", last)
	}
	for _, path := range []string{"config.json", "accounts"} {
		if _, err := os.Stat(filepath.Join(connection.dataDir, path)); !os.IsNotExist(err) {
			t.Fatalf("connection wrote %s: %v", path, err)
		}
	}
}

func TestRoleStatsThroughputQueueBound(t *testing.T) {
	a := testApp(t, nil)
	testRole(t, a, "connection", "http://control.invalid")
	now := time.Now()
	for i := 0; i < roleThroughputPendingLimit+2; i++ {
		a.queueRoleThroughputLocked(throughputMeasurement{CompletedAt: now.Add(time.Duration(i) * time.Second)})
	}
	if len(a.role.pendingThroughput) != roleThroughputPendingLimit || !a.role.pendingThroughput[0].CompletedAt.Equal(now.Add(2*time.Second)) {
		t.Fatal("queue did not drop oldest")
	}
}

func TestRoleStatsControlMerge(t *testing.T) {
	a, _ := roleFixture(t)
	now := time.Now().UTC()
	version := a.controlSnapshot().Version
	post := func(report roleStatsReport) {
		t.Helper()
		body, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if w := roleInternalRequest(a, "POST", "/internal/stats", a.role.secret, string(body)); w.Code != 204 {
			t.Fatalf("report status %d: %s", w.Code, w.Body.String())
		}
	}
	first := roleStatsReport{
		roleStatsCounters:  roleStatsCounters{2, 1, 1, 1, 1},
		PromptCache:        map[string]promptCacheStat{"slot:gpt-test:main": {AccountID: "slot", ModelID: "gpt-test", AgentKind: "main", RequestCount: 2, InputTokens: 100, CachedTokens: 50, UpdatedAt: now}, "deleted": {AccountID: "deleted", RequestCount: 10}},
		Health:             map[string]accountHealth{"slot": {LastSuccessAt: now, LastFailureAt: now.Add(-time.Second), LastFailureReason: "new", ConsecutiveFailure: 2}, "deleted": {LastSuccessAt: now}},
		RoutingCacheEvents: []routingCacheEvent{{AccountID: "deleted", Timestamp: now}},
		Throughput:         []*throughputMeasurement{{AccountID: "slot", StartedAt: now.Add(-time.Second), CompletedAt: now, Success: true, Usage: promptCacheUsage{Present: true, InputTokens: 100}}, {AccountID: "deleted", CompletedAt: now}, nil},
	}
	post(first)
	a.resetPromptCacheWindowLocked(now)
	second := roleStatsReport{
		roleStatsCounters: roleStatsCounters{3, 2, 1, 0, 1},
		PromptCache:       map[string]promptCacheStat{"slot:gpt-test:main": {AccountID: "slot", ModelID: "gpt-test", AgentKind: "main", RequestCount: 3, InputTokens: 200, CachedTokens: 70, UpdatedAt: now.Add(-time.Second)}},
		Health:            map[string]accountHealth{"slot": {LastSuccessAt: now.Add(time.Second), LastFailureAt: now.Add(-2 * time.Second), LastFailureReason: "old", ConsecutiveFailure: 9}},
		Throughput:        []*throughputMeasurement{{AccountID: "slot", StartedAt: now.Add(-2 * time.Second), CompletedAt: now, Success: true}},
	}
	for i := routingCacheEventLimit + 1; i >= 0; i-- {
		second.RoutingCacheEvents = append(second.RoutingCacheEvents, routingCacheEvent{AccountID: "slot", Timestamp: now.Add(-time.Duration(i) * time.Millisecond)})
	}
	second.RoutingCacheEvents = append(second.RoutingCacheEvents, routingCacheEvent{AccountID: "slot", Timestamp: now.Add(-routingCacheEventTTL - time.Second)})
	post(second)
	if a.state.RequestCount != 5 || a.state.SuccessCount != 3 || a.state.FailureCount != 2 || a.state.UpstreamResponseFailedCount != 1 || a.state.StreamIncompleteCount != 2 {
		t.Fatal("reporter counters did not add up")
	}
	stat := a.state.PromptCache["slot:gpt-test:main"]
	if len(a.state.PromptCache) != 1 || stat.RequestCount != 5 || stat.InputTokens != 300 || stat.CachedTokens != 120 || !stat.UpdatedAt.Equal(now) {
		t.Fatalf("prompt cache merge = %+v", a.state.PromptCache)
	}
	if a.promptCacheWindowLocked()["requestCount"].(uint64) != 3 {
		t.Fatal("report changed control reset baseline")
	}
	health := a.state.Health["slot"]
	if len(a.state.Health) != 1 || health.LastFailureReason != "new" || health.ConsecutiveFailure != 2 || !health.LastSuccessAt.Equal(now.Add(time.Second)) {
		t.Fatalf("health merge = %+v", health)
	}
	if len(a.state.RoutingCacheEvents) != routingCacheEventLimit {
		t.Fatal("routing cap not applied")
	}
	for i := 1; i < len(a.state.RoutingCacheEvents); i++ {
		if a.state.RoutingCacheEvents[i].Timestamp.Before(a.state.RoutingCacheEvents[i-1].Timestamp) {
			t.Fatal("routing events unordered")
		}
	}
	if len(a.throughputBuckets) != 1 || a.throughputBuckets[0].RequestCount != 2 {
		t.Fatalf("throughput merge = %+v", a.throughputBuckets)
	}
	if a.controlSnapshot().Version != version {
		t.Fatal("stats save churned snapshot version")
	}
	var persisted state
	if err := readJSON(filepath.Join(a.dataDir, "state", "runtime.json"), &persisted); err != nil || persisted.RequestCount != 5 {
		t.Fatalf("control persistence: %v", err)
	}
	post(roleStatsReport{Health: map[string]accountHealth{"slot": {LastFailureAt: now.Add(2 * time.Second), LastFailureReason: "newest", ConsecutiveFailure: 4}}})
	if a.state.Health["slot"].ConsecutiveFailure != 4 || a.state.Health["slot"].LastFailureReason != "newest" {
		t.Fatal("newest failure not merged")
	}
	post(roleStatsReport{Health: map[string]accountHealth{"slot": {LastFailureAt: now.Add(2 * time.Second), LastSuccessAt: now.Add(3 * time.Second)}}})
	if a.state.Health["slot"].ConsecutiveFailure != 0 || a.state.Health["slot"].LastFailureReason != "newest" {
		t.Fatal("success from newest failure reporter did not reset consecutive failures")
	}
}

func TestRoleStatsEndpointBoundary(t *testing.T) {
	a, _ := roleFixture(t)
	for _, secret := range []string{"", "wrong", a.role.secret} {
		if w := roleRequest(a, "POST", "/internal/stats", secret, "{}"); w.Code != 404 {
			t.Fatalf("public status = %d", w.Code)
		}
		if secret != a.role.secret {
			if w := roleInternalRequest(a, "POST", "/internal/stats", secret, "{}"); w.Code != 401 {
				t.Fatalf("unauthorized status = %d", w.Code)
			}
		}
	}
	for _, body := range []string{"{", "{} {}", `{"unused":"` + strings.Repeat("x", roleStatsBodyLimit) + `"}`} {
		if w := roleInternalRequest(a, "POST", "/internal/stats", a.role.secret, body); w.Code != 400 {
			t.Fatalf("invalid body status = %d", w.Code)
		}
	}
	if a.state.RequestCount != 0 {
		t.Fatal("invalid body changed stats")
	}
}
