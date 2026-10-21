# Control / connection MVP

For this host's production migration and rollback, use [MIGRATION.md](MIGRATION.md)
and `compose.production.yaml`. The base file preserves experiment ports/names;
production publishes only HAProxy, keeping the existing public UI and provider
URL together. `verify.sh` exercises routing and rolling upgrade on fresh
disposable Docker resources; it never uses an existing account volume.

Build `codex-pool:roles-v2` with the repository image script, copy `.env.example`
to `.env`, and replace all placeholders (different public/internal secrets).
Compose 2.24.4+ is required (production uses `!override`).
Validate with `docker compose -f deploy/roles/compose.yaml config -q`.
Building/testing this patch does not deploy anything.

```sh
CODEX_POOL_IMAGE=codex-pool:roles-v2 sh scripts/build-local-image.sh
```

Use that image for the roles stack so the admin UI shows the real build version
instead of `dev`.

**Before first startup the operator must stop the old `codex-exp-pool`.**
Control exclusively mounts the existing external `codex-exp-pool-data` volume.
Never run both credential writers or delete that volume. Start control first,
connections after control is healthy, then gateway, using Compose `create` /
`start`. No protected deployment is changed by this repository's tests.

Loopback ports: gateway `9100`, control UI/API `9180`, main diagnostics `9181`,
backup diagnostics `9182`. Control's credential broker is on the private-only
`8318` listener and is not published. The private bridge allows outbound
OAuth/Codex.
`CODEX_POOL_INTERNAL_ADDR` defaults to `0.0.0.0:8318`; connection nodes use
`CODEX_POOL_CONTROL_URL=http://control:8318`. Keep these ports aligned if changed.
The published control port returns 404 for `/internal/state`, `/internal/stats` and
`/internal/credentials/*`, even with the internal secret. The private listener
still requires that secret; `/internal/inflight` stays loopback-only on `8317`.
HAProxy's administrative socket `9999` is loopback-only inside the gateway and
is not published to the host; do not attach untrusted containers to the bridge.
Connection `/healthz` needs no key; provider routes still require API keys.
Connection admin writes/login return 404.

Only Codex device-auth slots are leased in this MVP. Provider API-key slots
remain available on control/monolith but are disabled in connection snapshots;
snapshots contain no API keys or OAuth tokens. Unknown JWT expiry is refused.
Use matching routing/prompt-cache/quota env settings across nodes.
Control's existing dashboard totals combine main and backup request statistics,
including Claude adapter traffic through backup; login/refresh races remain deferred.

After each successful 5s state poll, connections POST nonempty counter and
per-key prompt-cache deltas, account health, recent routing events and completed
throughput measurements to the secret-protected private `/internal/stats` endpoint
(8 MiB body limit). Delivery is at-least-once with about 5s delay plus request
time while healthy. Failed reports retain their baseline and retry accumulated
deltas; a timeout/crash after control accepts a report can cause double counting.
Acknowledged baselines persist with connection runtime, so restarts do not resend
lifetime totals. Control owns cache-reset baselines and ignores deleted-account
entries; statistics do not change the `/internal/state` snapshot version.
Throughput uses a memory-only pending queue capped at 4096 (drop oldest); restart
loses pending measurements, and control restart clears its chart history. Routing
events keep the existing TTL/cap, including during an outage. No per-node view,
bindings, cooldowns or ClientAgents are forwarded.

Polls run every 5s; disable/pending changes apply on the next successful poll.
During outages only cached leases serve, until token expiry or 30m since the
last successful snapshot (`CODEX_POOL_CONTROL_OFFLINE_MAX`), whichever is first.
Restart requires control; leases are never persisted. Each connection writes
only its own runtime volume, including identity markers for restart cleanup
and rotating service logs. It never writes config or auth files.

The image uses upstream's `supervisor.sh` and bundled `codex-upstream`; the
retired CLIProxyAPI sidecar and gateway-mode role guard are not used. Native
`/k/<key>/backend-api/codex/` provider routes are forwarded to connections, as
are the registered `/v1` provider routes. Messages/count-tokens reach the
Anthropic adapter (backed by connection-backup); everything else reaches control.
See [AUDIT.md](AUDIT.md) for the 14d5073 → 297a5dd call-site audit and limitations.

Upgrade control first, then connections (the new control must support the old
connections during the rollout). Prebuild/pull the image and set
`CODEX_ROLES_IMAGE=NEW_IMAGE` in `deploy/roles/.env` so subsequent Compose
commands keep the intended version. Do not run other Compose lifecycle commands
concurrently with the upgrade.

When migrating from the shared-listener version, also recreate connections with
the updated Compose configuration so they use `control:8318`. Old connections
pointing to `control:8317` lose polling and new leases as soon as control is
upgraded; only their bounded cached leases cover this migration window.

```sh
CODEX_ROLES_IMAGE=NEW_IMAGE docker compose -f deploy/roles/compose.yaml up -d --no-deps --wait control
sh deploy/roles/upgrade.sh NEW_IMAGE
```

Control restarts are covered by the running connections' offline tolerance:
only previously cached leases remain usable, until token expiry or
`CODEX_POOL_CONTROL_OFFLINE_MAX` (default 30m), whichever comes first. New leases
cannot be acquired while control is down. Wait for healthy control before
restarting connections, since their startup requires a fresh snapshot.

The script probes each new image on a disposable volume, drains/upgrades backup,
then drains/upgrades main; HAProxy fails back to main on its own. Each old
container gets 150s to stop and stays available for automatic rollback until
its replacement is healthy and HAProxy readiness succeeds. Only one container
runs on each runtime volume. Replacements are ordinary Compose service
containers, with the configured restart policy. Creating a stopped extra replica
preserves the old container; never replace that create with Compose up/start,
which could start both writers. Inspect leftovers manually after interruption.

For a stack left with one-off containers by the old script, run the fixed script
once (the current image is also valid). It safely replaces those containers in
the same backup-first order; no manual deletion or volume migration is needed.
Then `docker compose -f deploy/roles/compose.yaml up -d` and `ps` work again.
The first ordinary up can recreate the connections to reconcile the temporary
container-name override used during replacement; allow for connection restarts.
Use the rolling script when draining and rollback are required for an upgrade.

Manual drain (Docker only):

```sh
docker exec codex-roles-gateway sh -c 'echo "set server connections/connection-main state drain" | nc -w 2 127.0.0.1 9999'
```

Restore with `set server connections/connection-main state ready`. Failback is
automatic: once main passes 15 consecutive health checks (about 30s), new
requests go to main again; requests already on backup finish there. Never enable 5xx/timeout retries:
only pre-send `conn-failure` is retried. Drain stops new assignments; existing
HTTP/WS sessions can continue until the pool's 120s shutdown grace ends. Long
turns or idle sockets may disconnect and reconnect. Sticky/lineage state does
not migrate between connection nodes; each node reports its statistics to control.
