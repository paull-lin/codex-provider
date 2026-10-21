# Roles rebase audit: 14d5073 → 297a5dd

The comparison was read from this worktree's Git objects. No protected worktree,
running service, or existing data volume was used. The default role falls
through to the original 297a5dd code; no native request/header/body rewriting
was added. The role implementation remains in `role*.go`.

## Conflicts and removed sidecar

- `newAppFromEnv`: keep the rotating log setup, then `configureRole`, then `load`.
- `wsSession.startResponse`: add `!connectionAccountCurrent(bound)` to the new
  account-change predicate. Preserve `sent`: redial before the first send,
  close with 1012 after a send so Codex reconnects/resends in full.
- Upstream removed the sidecar, its credential reconciliation/sync functions,
  gateway model lookup, and `codexGatewayMode` field. Remove the obsolete role
  mode validation and tests. `rejectRetiredGatewayMode` still rejects obsolete
  settings globally. Do not restore `cliproxy-supervisor.sh` or its role guard.
- `docker-entrypoint.sh` now invokes `supervisor.sh`. The role compose file does
  not override either, so it inherits the correct entrypoint. Remove the dead
  direct-mode env setting and use the requested `codex-pool:roles-v2` image tag.

## Upstream call-site audit

Paths below are under `cmd/codex-pool/`. "Existing hook" means the hook in the
original roles patch; "new guard" means added during this rebase.

| New/changed upstream site | Connection behavior and coverage |
| --- | --- |
| `main.go:newAppFromEnv`, `logfile.go:newRotatingLogFile/open/Write` | Log setup precedes role setup. Logs remain on the node's own `/data/logs`; no config or auth access. The same rotation/permissions remain upstream's. |
| `main.go:load` | Existing early return to `loadConnection` reads only runtime and polls control. It never reads/normalizes local config or creates account credentials. Upstream deleted reconcile/sync entirely. |
| `main.go:serve`, `startQuotaRefresher` → `refreshAllCodexQuotas` | Existing role branch starts only the control snapshot poller. No quota sweep or scheduled prime on connection. |
| `main.go:sendQuotaPrimeRequest` → `codex_prime.go:primeWithCodexCLI` | New guard refuses before CLI lookup, lease lookup, raw auth read, temporary `auth.json`/`config.toml` creation or child execution. `runCodexExecPrime`/`codexChildEnvironment` are reached only on control/monolith. |
| `codex_own.go:sendCodexOwnRequest`, `codexBackendHeaders`, `codexJSON` | New central guard refuses all pool-owned maintenance traffic on connection. Native client passthrough uses `sendCodexBackend`, not this function. Control retains Codex transport, header order and JSON encoding unchanged. |
| `main.go:refreshCodexAuthContext` → `requestCodexTokenRefreshContext` | New local-refresh guard; normal forwarding uses the existing `refreshedCodexAuthContext` and `refreshCodexAuthAfterRejection` lease hooks. Direct token-endpoint callers are also stopped by the own-request guard. Only control rotates/persists tokens. |
| `main.go:activeCodexAuthContext` | Sidecar branch was removed upstream; delegates to `refreshedCodexAuthContext`, which leases on connection. |
| `main.go:readCodexAuthFile` | New raw-file guard refuses even if a stale local auth file exists. Routing/dashboard reads keep using the existing memory-only `codexAuth` hook. |
| `main.go:refreshAccountQuotaWithExpectedIdentity` → `sendCodexOwnRequest` | No connection poller, all mutating quota routes blocked by `roleHandler`; central own-request guard also rejects direct maintenance invocation. Connection receives authoritative quotas in snapshots. |
| `main.go:fetchCodexSubscriptionMetadata`, `codexAccountsCheckURL` | New `/wham/accounts/check` replaces browser/Node subscriptions lookups. It is part of control quota maintenance and passes the own-request guard. No extra connection metadata fetch. |
| `main.go:enrichResetCreditExpiration` → `fetchCodexResetCreditExpiration` | Control quota path only; the new own-request transport is guarded. Snapshot quota includes the resulting reset-credit metadata. |
| `main.go:handleAccountAction` → `consumeCodexResetCredit` | Mutating admin route is blocked before dispatch. New own-request guard independently refuses redemption from connection. No credits are consumed by tests. |
| `main.go:handleAccounts`, `handleAccountDelete` | Sidecar initialization/purge removed upstream. Existing role middleware denies all mutations, including login, public repair, notes and deletes. |
| `main.go:startLoginJobWithPolicyLocked` → `runLoginJob` | New creation guard also rejects direct owner/public repair callers before config changes or goroutine creation. Upstream CLI login/finalization stays on control; its removed sidecar steps remain removed. |
| `main.go:saveLocked`, `saveCompletedAuthVerificationLocked`, shutdown save | Existing normal-save hook plus new alternate-save guard write connection runtime only. Neither can write connection `config.json`. |
| `native_codex.go:registerNativeCodexRoutes/nativeCodexClient` | Existing API-key authentication and removal of client credentials unchanged. Responses/compact route through normal `forward` lease hooks; native WS uses handshake lease hooks. No native body rewrite added. |
| `native_codex.go:nativeCodexPassthroughAccount` | Uses snapshot config and normal routing eligibility (`usableLocked` → memory-only `codexAuth`), including thread/lineage selection. No auth-file reads or RPC under routing locks. |
| `native_codex.go:handleNativeCodexPassthrough` → `sendCodexBackend` | `models`, `images/generations`, `images/edits`, `memories/trace_summarize`, `alpha/search`, `realtime/calls` all acquire a lease through `refreshedCodexAuthContext`; one 401 retry uses `refreshCodexAuthAfterRejection`. Transport/header/body code unchanged. |
| `responses_ws.go:handleResponsesWebSocket/wsHandshakeModel/wsSession.dial` | New handshake-time routing selects from the snapshot. `dialCodexWebSocket` leases before opening/accepting the socket; 401 refresh goes to control. Same-account transport retries and upstream refusal semantics are retained. |
| `responses_ws.go:wsSession.startResponse/redial` | Same-slot identity change joins upstream's account-change predicate. First-send redial versus later reconnect semantics remain intact. |
| `responses_ws.go:noteTurnStateIssuer/noteEventTurnState/gateTurnState` | New issuer registry is slot-keyed. Role snapshot identity cleanup marks old issuers as replaced; simply deleting them would fall back to the newly bound sticky slot. Existing message gate then withholds old identity turn state. No upstream-file hook added for this. |
| `codex_own.go:noteClientAgent/clientAgentFor`, callers `forwardOnce`, `sendCodexBackend`, `dialCodexWebSocketOnce` | Agent labels are node-local runtime metadata, saved through the existing runtime hook. No credential material is recorded. Control does not receive connection client-agent telemetry in this MVP; see limitation below. |
| `main.go:handleProxy/forwardOnce`, transport retry and native response-header changes | Credential acquisition/rejection remains behind existing lease hooks; retries reuse those paths. New routing/unknown-failure/client-entry counters persist via `saveLocked` to runtime only. |
| `main.go:usableLocked/proTierLocked`, preference/balancing and plan/weight changes | Operate on snapshot account/quota metadata and existing memory-only credential eligibility. No new account interpretation is introduced by roles; routing/dashboard retain upstream's shared definitions. |
| Deleted `reconcileCodexCredential`, `syncCliproxyAuth*`, `cliproxyCodexAuth`, `updateCliproxyAuthMetadata`, gateway catalog and browser metadata helpers | No residual callers or role guards required. No replacement credential copy is introduced. |

## Integration size and regression coverage

There are **16 behavioral integration sites** in upstream Go files:
13 in `main.go`, one in `responses_ws.go`, one in `codex_own.go`, and one in
`codex_prime.go`, plus the `app.role` field. Nine sites came from the original
roles patch; six new early-return guards protect raw auth read, local refresh,
login creation, alternate save, own requests and CLI priming. The new turn-state
cleanup stays in `role_connection.go`. `native_codex.go`, `logfile.go`, the
supervisor and Dockerfile need no changes.

The statistics forwarding change adds exactly one upstream-file hook:
`main.go:recordThroughputMeasurementLocked` calls `queueRoleThroughputLocked`
only on connection nodes, retaining the existing local chart path. All delta,
bounded queue, persistence, private endpoint and merge logic lives in `role_*.go`.
The queue is memory-only, capped at 4096, and drops oldest measurements.
Control writes stats to runtime.json only, never config.json, so the
authority snapshot version does not churn. No upstream request,
header, transport, parsing, classification or retry behavior changes.

Role tests cover snapshot sanitization/persistence, lease/CAS refresh, outage
bounds, identity cleanup/restart, admin denial, direct control-only boundaries,
HTTP/WS/native models/responses/compact 401 handling, and native WS handshake
leasing plus identity replacement before/after the first send. Upstream tests
continue to cover the unchanged routing, native forwarding and wire shapes.

Validation on 2026-10-20 (Docker only): `gofmt`, `go vet ./...` and
`go test ./...` succeeded (pool package: 187.358s); the requested
`docker build -t codex-pool:roles-v2 .` and compose `config -q` succeeded.
`go test -race ./cmd/codex-pool -run 'Test(Connection|Role)' -count=1` also
succeeded (142.106s).
HAProxy `-c` and upgrade script `sh -n` succeeded; standalone HAProxy validation
has no Compose DNS, so its two server-name resolution notices are expected.

Codex conformance checked 28 watched paths, 28 shapes, 17 assumptions and
19 codes. Wire capture passed own calls, prime, HTTP/WS, native endpoint,
subagent, compaction and failover comparisons. Its report is in the ignored
`scripts/conformance/out/wire-20261020T104244Z/report.txt`. To honor the volume
restriction, temporary copies of the existing harness scripts used fresh
`/tmp` bind directories instead of Docker volumes, and a separate conformance
image tag. The shape-check copy also ran Git initialization from `/tmp` because
this worktree's `.git` pointer is outside its isolated container. No comparison
logic, watched source, wire fixture or acceptance criterion was changed.

Statistics validation on 2026-10-21 (Docker only): `gofmt`, `go vet ./...`,
`go test ./...` (203.446s), and `go test -race ./cmd/codex-pool -run TestRoleStats
-count=1` (40.366s) passed. `docker build -t codex-pool:roles-stats .` passed.
The isolated Compose project `roles-stats-verify-20261021-a` used fresh volumes,
an internal network, random loopback ports, synthetic credentials, and a mock
using pinned Codex response/usage fixtures. Gateway traffic produced two main
and four backup successes (one through the existing Claude adapter image).
Control reported six requests, 600 input/240 cached/360 cache-write tokens, six
throughput measurements and six routing events. Cache reset retained a one-request
window, connection restarts did not recount totals, and stats reports preserved
the snapshot version. Public stats returned 404; the private endpoint required
the secret. No connection config/auth files were created. The throwaway stack,
its three volumes and network were removed; protected deployments were untouched.

## Limits before deployment

- No live account data or real provider calls were used; no deployment, upgrade,
  drain, existing container/volume operation, commit or push was performed.
- Connection `ClientAgents` do not travel back to control. Control own calls
  therefore use its own last observed agent or upstream's pinned-CLI fallback,
  not necessarily the latest client seen by a connection. Client-agent telemetry
  propagation remains outside this statistics patch.
- Existing MVP limits remain: polling delays revocation, control is not HA,
  in-flight work is not cancelled by snapshots, and routing/socket
  state do not migrate between connections. Existing control login/refresh races
  are not redesigned here.
- The wire harness exercises default-role native wire compatibility. Role
  routing/credentials are verified by local broker/fake-upstream tests; a live
  multi-node rollout is still unverified.
