# Production monolith → roles, and rollback

This runbook is for the operator of this host. It moves the existing production
service to roles without account re-login, then verifies the unchanged public
URL. These commands are **operator actions**, not actions performed when editing
or testing this repository. Do not run them against the experiment project.

| Item | Before | After |
| --- | --- | --- |
| Worktree | `~/workspace/codex-provider` | `~/workspace/codex-provider-prod` |
| Project | `codex-provider` | `codex-production-roles` |
| Credential/config writer | `codex-pool` | `codex-production-roles-control` |
| Credential volume | `codex-pool-data` | same external volume, control only |
| Published listener | `0.0.0.0:8317` monolith | `0.0.0.0:8317` HAProxy |
| Control / broker | monolith | unpublished `8317` / unpublished `8318` |
| Connection state | monolith runtime | separate main/backup runtime volumes |
| Public URL | `https://codex-pool-api.paullin.tw` | unchanged |

Cloudflare keeps forwarding to `http://localhost:8317`; no tunnel edits are
needed. `/`, `/admin*`, dashboard assets/APIs and public `/healthz` reach control.
The gateway routes these exact provider paths to main, with backup as HAProxy's
`backup` server:

- `/v1/responses` (POST and WebSocket GET), `/v1/responses/compact`,
  `/v1/chat/completions`, and `/v1/models`.
- `/v1/codex-pool/status` is a control status API and therefore follows the
  catch-all control route, together with all other non-provider paths.
- `/k/<key>/backend-api/codex/` followed by `responses` (POST/WS GET),
  `responses/compact`, `models`, `images/generations`, `images/edits`,
  `memories/trace_summarize`, `alpha/search`, or `realtime/calls`.
- `/v1/messages` and `/v1/messages/count_tokens` reach the adapter, whose pool
  URL is always `http://connection-backup:8317`.

All other paths go to control. Query strings do not change routing. No path,
body, auth or WebSocket header rewrite is added. Only pre-send connect failures
permit HAProxy redispatch; HTTP errors/timeouts are never replayed. Do not enable
HAProxy HTTP request logging: native paths contain a client API key.

## 1. Preflight (no production stop yet)

Use a dedicated shell and run from the production worktree. Docker Compose
2.24.4+ is required for `!override`. All build/test tooling runs in Docker. The
shell scripts below only orchestrate Docker and prepare files.

```sh
cd ~/workspace/codex-provider-prod
set -eu
docker compose version
CODEX_POOL_IMAGE=codex-pool:production-roles sh scripts/build-local-image.sh
docker build -t anthropic-adapter:production-roles -f adapters/anthropic/Dockerfile .
docker pull haproxy:3.2-alpine
```

Keep both new image tags immutable through the cutover/rollback window. Save the
old container's image ID; do not recreate/delete that container or overwrite its
image tag. Check the old and new bundled CLI versions are both `0.163.0` (the pin
in `upstream/Cargo.toml`), and confirm the old pool uses the same upstream
`297a5dd` data format. A different upstream/schema version needs a separate
migration review; matching CLI versions alone does not prove schema compatibility.

```sh
umask 077
docker inspect -f '{{.Image}}' codex-pool > deploy/roles/rollback-image.txt
docker exec codex-pool codex --version
docker run --rm --entrypoint codex codex-pool:production-roles --version
docker volume inspect -f '{{.Name}}' codex-pool-data
docker inspect -f '{{range .Mounts}}{{println .Name .Destination}}{{end}}' codex-pool
```

Prepare an ignored env file by copying the monolith's env **without displaying
its contents**. This preserves API keys, the admin password hash, dashboard and
routing/cache/quota settings. Do not source it as shell code. Do not regenerate
the public key or admin hash. Start with a new destination; do not append these
settings repeatedly. A fresh internal secret is generated in Docker.

```sh
test ! -e deploy/roles/.env.production
cp ~/workspace/codex-provider/.env deploy/roles/.env.production
chmod 600 deploy/roles/.env.production
docker run --rm --entrypoint node codex-pool:production-roles -e \
  'console.log("\nCODEX_POOL_INTERNAL_SECRET="+require("crypto").randomBytes(32).toString("hex"))' \
  >> deploy/roles/.env.production
cat >> deploy/roles/.env.production <<'ENV'
CODEX_ROLES_PROJECT=codex-production-roles
CODEX_ROLES_CONTROL_VOLUME=codex-pool-data
CODEX_ROLES_PORT=8317
CODEX_ROLES_IMAGE=codex-pool:production-roles
ANTHROPIC_ADAPTER_IMAGE=anthropic-adapter:production-roles
CODEX_POOL_INTERNAL_ADDR=0.0.0.0:8318
ENV
```

Review the file locally without pasting it into chat/logs. Preserve valid Compose
quoting (especially any `$` in secrets); remove duplicate obsolete role/internal
settings. Roles override listener, role, data directory and shutdown grace.
If the old config references key files or other host mounts, reproduce those
mounts explicitly before proceeding; this recipe assumes the stated API key and
admin hash are in `.env`. Do not copy upstream-account auth files: the existing
external volume already holds them. Never commit the env or account data.

Select the same files for **every** subsequent Compose/upgrade command. Run this
block again in any new shell; it also prevents accidental experiment selection.

```sh
cd ~/workspace/codex-provider-prod
export COMPOSE_PROJECT_NAME=codex-production-roles
export CODEX_ROLES_ENV_FILE="$PWD/deploy/roles/.env.production"
export COMPOSE_ENV_FILES="$CODEX_ROLES_ENV_FILE"
export COMPOSE_FILE="$PWD/deploy/roles/compose.yaml:$PWD/deploy/roles/compose.production.yaml"
roles() { docker compose --project-directory "$PWD/deploy/roles" "$@"; }
roles config -q
docker run --rm -v "$PWD/deploy/roles/haproxy.cfg:/usr/local/etc/haproxy/haproxy.cfg:ro" \
  haproxy:3.2-alpine haproxy -c -f /usr/local/etc/haproxy/haproxy.cfg
```

Standalone HAProxy validation may report unresolved Compose DNS names; it must
still exit zero. Never dump unredacted `compose config` (it contains secrets).
Confirm the selected model exposes **only gateway port 8317**, the external
control volume is `codex-pool-data`, and runtime/network names have the
`codex-production-roles` prefix. This prints no environment values:

```sh
roles config --format json | docker run --rm -i node:22-bookworm-slim node -e '
let raw="";process.stdin.on("data",c=>raw+=c).on("end",()=>{
 const c=JSON.parse(raw), a=require("assert/strict");
 a.equal(c.name,"codex-production-roles");
 a.equal(c.volumes["control-data"].name,"codex-pool-data");
 a.equal(c.volumes["control-data"].external,true);
 for(const n of ["control","connection-main","connection-backup","anthropic-adapter"]) a.equal((c.services[n].ports||[]).length,0);
 a.deepEqual(c.services.gateway.ports.map(p=>[p.host_ip,p.published,p.target]),[["0.0.0.0","8317",9100]]);
 for(const n of ["main-runtime","backup-runtime"]) a.ok(c.volumes[n].name.startsWith(c.name+"-"));
 a.equal(c.networks.internal.name,c.name+"-internal");
 console.log("Production names, volume and exposure verified");
});'
```

Before the maintenance window, run the isolated checks below with these images.
They use a fresh empty control volume, never `codex-pool-data`:

```sh
CODEX_ROLES_TEST_IMAGE=codex-pool:production-roles \
CODEX_ROLES_TEST_ADAPTER_IMAGE=anthropic-adapter:production-roles \
  sh deploy/roles/verify.sh
docker run --rm -v "$PWD:/src:ro" -w /src golang:1.24 \
  sh -ceu 'go vet ./...; go test ./...'
```

This deployment change does not alter the pool's upstream wire implementation.
For any image that also changes upstream traffic, the repository deployment gate
still applies: Go tests, `sh scripts/conformance/codex-conformance.sh check` and
`sh scripts/conformance/wire-capture.sh` must pass on that exact revision before
deploying. The conformance scripts use their own caches; do not use existing
volumes during a verification session that forbids them.

In the current public UI, record the expected **7 usable Codex device-auth
accounts** and verify there are no active login/repair jobs. Provider-key slots
cannot serve on connections. Do not record account emails/tokens in the runbook.
Ensure the original monolith is the only running writer of `codex-pool-data`.
Schedule a quiet window and pause new client requests and admin mutations.

## 2. Cut over at an idle moment

The loopback-only probe counts active HTTP requests and active WS turns, not idle
WebSocket connections. This follows `scripts/deploy.sh`'s in-container probe,
but fails closed after ten minutes instead of forcing a busy migration. A race
with a new request is still possible; the 120s application grace and 150s Docker
stop timeout are the backstop. Idle sockets reconnect during the outage.

```sh
in_flight() {
  docker exec "$1" node -e '
    fetch("http://127.0.0.1:8317/internal/inflight", {signal:AbortSignal.timeout(2000)})
      .then(r=>r.ok?r.json():Promise.reject(new Error(String(r.status))))
      .then(b=>{if(!Number.isInteger(b.inFlight)||b.inFlight<0)process.exit(1);console.log(b.inFlight)})
      .catch(()=>process.exit(1))'
}
wait_idle() (
  waited=0
  while :; do
    count=$(in_flight "$1") || exit 1
    [ "$count" = 0 ] && exit 0
    [ "$waited" -lt 600 ] || { echo 'Still busy; aborting migration' >&2; exit 1; }
    sleep 1
    waited=$((waited + 1))
  done
)
wait_idle codex-pool && docker stop -t 150 codex-pool
test "$(docker inspect -f '{{.State.Running}}' codex-pool)" = false
```

**Stop on any error.** Do not start control until the old writer is confirmed
stopped. Do not run the old project's `compose up` while roles are active. The
old container stays stopped and retains its original image/config for rollback.
The public URL is temporarily unavailable from this point until gateway starts.

```sh
roles create --no-build
roles start --wait --wait-timeout 120 control
roles start --wait --wait-timeout 120 connection-main connection-backup
roles start anthropic-adapter
roles start gateway
roles ps
roles exec -T gateway haproxy -c -f /usr/local/etc/haproxy/haproxy.cfg
roles exec -T control node -e '
fetch("http://127.0.0.1:8317/healthz",{headers:{Authorization:"Bearer "+process.env.CODEX_POOL_API_KEY},signal:AbortSignal.timeout(3000)})
 .then(r=>{if(!r.ok)throw Error(String(r.status));console.log("control healthy")}).catch(()=>process.exit(1))'
for service in connection-main connection-backup; do
  roles exec -T "$service" node -e '
fetch("http://127.0.0.1:8317/healthz",{signal:AbortSignal.timeout(3000)})
 .then(r=>{if(!r.ok)throw Error(String(r.status));console.log("connection healthy")}).catch(()=>process.exit(1))'
done
```

Wait a few seconds for HAProxy checks, then run the public URL smoke test from
the control container. It inherits the existing key without printing it. It
performs **two real inference requests**, which consume quota; `max_tokens` is
not enforced by the adapter. It checks Responses completion, a real upstream
WebSocket 101 handshake, Claude Messages, health, login page, root and dashboard.

```sh
roles exec -T -e CODEX_SMOKE_ORIGIN=https://codex-pool-api.paullin.tw \
  control node < deploy/roles/smoke.cjs
```

Open `https://codex-pool-api.paullin.tw/admin` in a browser, log in using the
existing admin password, and confirm the expected accounts are still usable.
Confirm the root/public dashboard and its assets render via that same URL.
The automated smoke checks the login page; the browser step verifies the real
password/cookie/Cloudflare flow. If any acceptance check fails, stop accepting
traffic and use rollback below. Do not work around a failure by re-logging in
all accounts or publishing control/broker ports.

Inspect the dashboard's **Unrecognized failures** and each node's log for
`class=unknown` after smoke and initial client traffic. Control does not receive
connection request statistics, so its dashboard alone is insufficient:

```sh
roles logs --since 15m control connection-main connection-backup | \
  docker run --rm -i alpine:3.21 sh -c 'grep "class=unknown" || test "$?" = 1'
```

Any occurrence requires investigation against the pinned Codex source/fixtures;
do not treat it as a successful rollout. Avoid sharing raw logs publicly.

## 3. Roll back to the retained monolith

Use the selection block from preflight in a fresh shell. Reuse the `in_flight`
and `wait_idle` functions from cutover. Pause new clients and admin changes;
wait for both connection nodes to finish active turns if they are healthy:

```sh
wait_idle codex-production-roles-conn-main
wait_idle codex-production-roles-conn-backup
# Stops ingress first; stop connections before the sole credential writer.
roles stop -t 150 gateway anthropic-adapter
roles stop -t 150 connection-main connection-backup
roles stop -t 150 control
test "$(docker inspect -f '{{.State.Running}}' codex-production-roles-control)" = false
test "$(docker inspect -f '{{.Image}}' codex-pool)" = "$(cat deploy/roles/rollback-image.txt)"
docker start codex-pool
```

If a broken connection cannot report idle, the operator may proceed with these
graceful stops during the maintenance window, accepting disconnected turns.
If a stop fails, **do not start the monolith** until control is confirmed stopped.
Do not use `down -v`, delete volumes, copy old auth files back, or recreate the old
container from a mutable tag. Keep the stopped roles stack for diagnosis.

Check the old listener and the unchanged public URL:

```sh
docker exec codex-pool node -e '
fetch("http://127.0.0.1:8317/healthz",{headers:{Authorization:"Bearer "+process.env.CODEX_POOL_API_KEY},signal:AbortSignal.timeout(3000)})
 .then(r=>{if(!r.ok)throw Error(String(r.status));console.log("monolith healthy")}).catch(()=>process.exit(1))'
docker exec codex-pool node -e '
fetch("https://codex-pool-api.paullin.tw/admin",{signal:AbortSignal.timeout(10000)})
 .then(r=>{if(!r.ok)throw Error(String(r.status));console.log("public admin reachable")}).catch(()=>process.exit(1))'
```

Use the existing Codex client for a short Responses/WS turn and log in through
the browser. Claude `/v1/messages` is not part of the old monolith and is
unavailable after rollback. No Cloudflare changes are needed in either direction.

Rollback is data-compatible **under the same-upstream/schema preflight gate**:
roles introduce no config/auth schema migration; control uses the monolith's
existing writer. Connections persist only to their own runtime volumes. Control
may have refreshed tokens during the rollout; the restarted monolith reads those
latest files from the same volume. Restoring an old credential backup could
resurrect invalid rotated refresh tokens, so do not do that. Connection-local
request/sticky statistics do not merge back into the monolith.

## 4. Subsequent upgrades and known limits

Keep the selection exports from preflight. Prebuild a new image, update
`CODEX_ROLES_IMAGE` in the ignored production env, then upgrade control before
connections:

```sh
roles up -d --no-deps --no-build --wait control
sh deploy/roles/upgrade.sh YOUR_PREBUILT_IMAGE
```

The script discovers containers in the selected project, preserves production
overrides, probes on a disposable volume, and drains/upgrades backup then main;
HAProxy fails back to main automatically after ~30s of healthy checks. Do not run other lifecycle commands concurrently. Its
old connection is stopped before its replacement starts on the runtime volume.
Inspect interrupted leftovers before rerunning. HAProxy's admin socket remains
`127.0.0.1:9999` **inside gateway**, never a host port:

```sh
roles exec -T gateway sh -c 'echo "set server connections/connection-main state drain" | nc -w 2 127.0.0.1 9999'
# Undo the drain (failback itself is automatic):
roles exec -T gateway sh -c 'echo "set server connections/connection-main state ready" | nc -w 2 127.0.0.1 9999'
```

- Request statistics (counts, prompt cache, health, routing events, throughput)
  are reported to control every 5s and shown as main + backup totals in the
  control UI. Client-agent data and sticky/lineage/WS state stay on connection
  nodes and do not migrate during failover.
- Adapter traffic goes directly to backup and does not obey the HAProxy
  `connections` backend's drain state. Backup upgrades can interrupt Claude
  traffic; schedule a quiet window for it. There is no adapter failover.
- Control is not HA. Existing cached leases are bounded by token expiry and
  `CODEX_POOL_CONTROL_OFFLINE_MAX` (default 30m); new/restarted connections need
  control. Config/revocation polling takes about 5s. Login/refresh races remain
  the existing MVP limitation.
- The 150s Docker stop timeout encloses the 120s pool grace; long turns and idle
  WS sockets may disconnect. Adapter has no equivalent application drain.
- Public `/healthz` checks control, not the whole stack. Check both connections,
  adapter and public inference separately. HAProxy uses TCP health for control
  to avoid embedding the API key in its config; Compose checks authenticated
  control readiness.
- Admin/dashboard auth rules are unchanged. Proxy trust/client-IP configuration
  is inherited from the monolith; validate login throttling behind the new hop
  before changing trusted-proxy settings. Never trust arbitrary forwarded IPs.
- The experiment defaults remain `codex-roles`, `codex-exp-pool-data`, gateway
  `127.0.0.1:9100`, and diagnostics `9180–9182`. Production requires the override
  and explicit env selection; never invoke the experiment defaults by accident.
- Local tests prove gateway routing, WS tunneling, adapter-to-backup wiring,
  real empty-state admin login and rolling upgrade. They do not prove production
  credentials, real account eligibility, Cloudflare behavior or live inference.

## Reproducible isolated verification

```sh
CODEX_POOL_IMAGE=codex-pool:production-verify sh scripts/build-local-image.sh
docker build -t anthropic-adapter:production-verify -f adapters/anthropic/Dockerfile .
sh deploy/roles/verify.sh
```

The harness uses `codex-roles-verify-<timestamp>-<pid>`, host port `19317`
(`CODEX_ROLES_TEST_PORT` can change it), an empty external test control volume and
new runtime volumes. It replaces pool nodes with labeled fixtures for exact
backend assertions and bidirectional WS checks, then runs real pool images with
no accounts for admin/auth/upgrade checks. It never mounts production/experiment
volumes or reads their env files. Successful completion asserts all test
containers, volumes and networks are gone; built images remain as build artifacts.
