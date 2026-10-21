#!/bin/sh
# Docker-only, disposable integration test. Never reads a deployment .env and
# never copies account data. Names must be new before cleanup is armed.
set -eu
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
export CODEX_ROLES_IMAGE=${CODEX_ROLES_TEST_IMAGE:-codex-pool:production-verify}
export ANTHROPIC_ADAPTER_IMAGE=${CODEX_ROLES_TEST_ADAPTER_IMAGE:-anthropic-adapter:production-verify}
export COMPOSE_PROJECT_NAME=codex-roles-verify-$(date +%s)-$$
export CODEX_ROLES_PROJECT=$COMPOSE_PROJECT_NAME
export CODEX_ROLES_CONTROL_VOLUME=$COMPOSE_PROJECT_NAME-empty-control
export CODEX_ROLES_PORT=${CODEX_ROLES_TEST_PORT:-19317}
case "$CODEX_ROLES_PORT" in
  8317|8318|9100|9180|9181|9182) echo 'Refusing a protected deployment port' >&2; exit 1 ;;
esac
export COMPOSE_FILE=$DIR/compose.yaml:$DIR/compose.production.yaml
BASE_FILES=$COMPOSE_FILE
TEST_DIR=$(mktemp -d)
export CODEX_ROLES_ENV_FILE=$TEST_DIR/test.env
export COMPOSE_ENV_FILES=$CODEX_ROLES_ENV_FILE
umask 077
printf '%s\n' 'CODEX_POOL_API_KEY=throwaway-test-key' 'CODEX_POOL_INTERNAL_SECRET=throwaway-internal-only' > "$CODEX_ROLES_ENV_FILE"
hash=$(docker run --rm --entrypoint codex-pool -e CODEX_POOL_ADMIN_PASSWORD=throwaway-test-password "$CODEX_ROLES_IMAGE" hash-password)
printf "CODEX_POOL_ADMIN_PASSWORD_HASH='%s'\n" "$hash" >> "$CODEX_ROLES_ENV_FILE"
compose() { docker compose --project-directory "$DIR" "$@"; }
check() { docker run --rm --network "$COMPOSE_PROJECT_NAME-internal" -v "$DIR/test:/test:ro" node:22-bookworm-slim node /test/check.cjs "$@"; }
admin() { docker exec "$COMPOSE_PROJECT_NAME-gateway" sh -c 'printf "%s\n" "$1" | nc -w 2 127.0.0.1 9999' sh "$1"; }
if [ -n "$(docker ps -aq --filter "label=com.docker.compose.project=$COMPOSE_PROJECT_NAME")" ] ||
  docker volume inspect "$CODEX_ROLES_CONTROL_VOLUME" >/dev/null 2>&1 ||
  docker network inspect "$COMPOSE_PROJECT_NAME-internal" >/dev/null 2>&1; then
  echo 'Refusing existing test resources' >&2
  exit 1
fi
cleanup() {
  COMPOSE_FILE=$BASE_FILES compose down -v --remove-orphans
  docker volume rm "$CODEX_ROLES_CONTROL_VOLUME"
  rm -rf "$TEST_DIR"
}
docker volume create "$CODEX_ROLES_CONTROL_VOLUME" >/dev/null
trap cleanup EXIT
trap 'exit 1' INT TERM
echo "Disposable project: $COMPOSE_PROJECT_NAME; port: $CODEX_ROLES_PORT; volume: $CODEX_ROLES_CONTROL_VOLUME"
compose config -q
compose config --format json | docker run --rm -i node:22-bookworm-slim node -e '
let raw="";process.stdin.on("data",c=>raw+=c).on("end",()=>{
 const c=JSON.parse(raw), a=require("assert/strict");
 for(const name of ["control","connection-main","connection-backup","anthropic-adapter"]) a.equal((c.services[name].ports||[]).length,0);
 a.equal(c.services.gateway.ports.length,1);
 a.equal(c.services.gateway.ports[0].target,9100);
 a.equal(c.volumes["control-data"].external,true);
 console.log("PASS production config: gateway only published; control external volume");
});'
docker run --rm -v "$DIR:/roles:ro" --entrypoint sh node:22-bookworm-slim -n /roles/upgrade.sh
export COMPOSE_FILE=$BASE_FILES:$DIR/test/mock.yaml
compose up -d --no-build --wait --wait-timeout 90
compose exec -T gateway haproxy -c -f /usr/local/etc/haproxy/haproxy.cfg
check mock
docker run --rm --network host -e TEST_PORT="$CODEX_ROLES_PORT" node:22-bookworm-slim node -e '
fetch("http://127.0.0.1:"+process.env.TEST_PORT+"/admin",{signal:AbortSignal.timeout(3000)})
 .then(async r=>{require("assert/strict").equal((await r.json()).backend,"control");console.log("PASS published test port routes admin to control")})
 .catch(e=>{console.error(e);process.exit(1)});'
# Close only this disposable primary: exercise connect failure, backup while
# main is not yet stable (rise 15 x 2 s), then automatic failback.
compose stop -t 1 connection-main
check selected connection-backup
compose start connection-main
sleep 5
check selected connection-backup
failback=""
for attempt in $(seq 1 30); do
  sleep 2
  if check selected connection-main >/dev/null 2>&1; then failback=$((5 + 2 * attempt)); break; fi
done
[ -n "$failback" ] || { echo 'FAIL main did not take traffic back' >&2; exit 1; }
echo "PASS automatic failback to main about ${failback}s after restart"
# upgrade.sh relies on drain alone (no stick table) to move new requests.
admin 'set server connections/connection-main state drain'
check selected connection-backup
admin 'set server connections/connection-main state ready'
check selected connection-main
compose down -v
export COMPOSE_FILE=$BASE_FILES
# Exercise the exact create/start ordering documented for production cutover.
compose create --no-build
compose start --wait --wait-timeout 120 control
compose start --wait --wait-timeout 120 connection-main connection-backup
compose start anthropic-adapter
compose start gateway
sleep 3
check real
before_main=$(compose ps -q connection-main)
before_backup=$(compose ps -q connection-backup)
sh "$DIR/upgrade.sh" "$CODEX_ROLES_IMAGE"
[ "$(compose ps -q connection-main)" != "$before_main" ]
[ "$(compose ps -q connection-backup)" != "$before_backup" ]
for service in connection-main connection-backup; do
  id=$(compose ps -q "$service")
  [ "$(docker inspect -f '{{index .Config.Labels "com.docker.compose.oneoff"}}' "$id")" = False ]
  [ "$(docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$id")" = unless-stopped ]
  [ "$(docker inspect -f '{{len .HostConfig.PortBindings}}' "$id")" = 0 ]
done
check real
# The next ordinary Compose up must still work after the rolling replacement.
compose up -d --no-build --wait --wait-timeout 120
check real
docker run --rm --network "$COMPOSE_PROJECT_NAME-internal" node:22-bookworm-slim node -e '
const net=require("net");const s=net.connect(9999,"gateway");
s.setTimeout(3000,()=>process.exit(1));
s.on("connect",()=>process.exit(1));s.on("error",e=>{if(e.code!=="ECONNREFUSED")process.exit(1);console.log("PASS admin socket unavailable from private bridge");});'
echo 'PASS rolling upgrade, ordinary compose reconciliation, unpublished replacement ports'
cleanup
trap - EXIT
[ -z "$(docker ps -aq --filter "label=com.docker.compose.project=$COMPOSE_PROJECT_NAME")" ]
[ -z "$(docker volume ls -q --filter "name=$COMPOSE_PROJECT_NAME")" ]
[ -z "$(docker network ls -q --filter "name=$COMPOSE_PROJECT_NAME")" ]
echo 'PASS teardown: no test containers, volumes or network remain'
