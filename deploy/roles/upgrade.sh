#!/bin/sh
set -eu

# Only connection containers in this project; never mount control's volume.
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
: "${1:?usage: sh deploy/roles/upgrade.sh NEW_IMAGE}"
export CODEX_ROLES_IMAGE=$1
# Honor the same COMPOSE_FILE/COMPOSE_ENV_FILES/project selection as deployment.
# Derive container names from Compose so a production or test upgrade can never
# fall back to the experiment's hard-coded containers or probe volume.
export COMPOSE_FILE=${COMPOSE_FILE:-$DIR/compose.yaml}

compose() { docker compose --project-directory "$DIR" "$@"; }
GATEWAY=$(compose ps -q gateway)
MAIN=$(compose ps -q connection-main)
BACKUP=$(compose ps -q connection-backup)
for id in "$GATEWAY" "$MAIN" "$BACKUP"; do
  [ -n "$id" ] || { echo 'Selected project must have a running gateway and both connections' >&2; exit 1; }
done
PROJECT=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$GATEWAY")
MAIN=$(docker inspect --format '{{.Name}}' "$MAIN" | sed 's|^/||')
BACKUP=$(docker inspect --format '{{.Name}}' "$BACKUP" | sed 's|^/||')
NEXT=$PROJECT-upgrade-next
PROBE=$PROJECT-upgrade-probe
PROBE_VOLUME=$PROJECT-upgrade-probe-data
gateway() {
  reply=$(docker exec "$GATEWAY" sh -c 'printf "%s\n" "$1" | nc -w 2 127.0.0.1 9999' sh "$1") || return 1
  # HAProxy reports command errors in the response even when nc exits zero.
  [ -z "$(printf '%s' "$reply" | tr -d '\r\n')" ] || { printf '%s\n' "$reply" >&2; return 1; }
}
healthy() {
  for attempt in $(seq 1 30); do
    status=$(docker inspect --format '{{.State.Health.Status}}' "$1")
    [ "$status" = healthy ] && return 0
    [ "$(docker inspect --format '{{.State.Running}}' "$1")" = true ] || return 1
    sleep 2
  done
  return 1
}
drain() {
  gateway "set server connections/$1 state drain; clear table connections"
}
ready() { gateway "set server connections/$1 state ready"; }

ROLLBACK=""
CANDIDATE=$NEXT
rollback() {
  if [ -n "$ROLLBACK" ]; then
    docker rm -f "$CANDIDATE" >/dev/null 2>&1 || true
    if [ "$ROLLBACK" != "$old" ]; then docker rename "$ROLLBACK" "$old"; fi
    docker start "$old" >/dev/null
    ready "$service"
    echo "Upgrade aborted; old container retained and restarted" >&2
  fi
}
trap rollback EXIT
trap 'exit 1' INT TERM

# Leftovers may be the only healthy replacement after interruption; never
# delete them blindly. Inspect manually before retrying an interrupted run.
for name in "$NEXT" "$PROBE" "$MAIN-previous" "$BACKUP-previous"; do
  if docker container inspect "$name" >/dev/null 2>&1; then
    echo "Existing $name; inspect the interrupted upgrade before continuing" >&2
    exit 1
  fi
done
docker image inspect "$CODEX_ROLES_IMAGE" >/dev/null
healthy "$MAIN"
healthy "$BACKUP"

replace() {
  service=$1
  old=$2
  # Probe a disposable volume while old stays running. Never run two writers
  # on the real runtime volume, even during candidate health validation.
  compose run -d --no-deps --name "$PROBE" -v "$PROBE_VOLUME:/data" "$service" >/dev/null
  if ! healthy "$PROBE"; then
    docker rm -f "$PROBE" >/dev/null
    docker volume rm "$PROBE_VOLUME" >/dev/null
    echo "New image failed health check; old container kept" >&2
    return 1
  fi
  docker rm -f "$PROBE" >/dev/null
  docker volume rm "$PROBE_VOLUME" >/dev/null
  drain "$service"
  CANDIDATE=$NEXT
  ROLLBACK=$old
  docker stop -t 150 "$old" >/dev/null
  # Compose labels are immutable: run/rename leaves one-off containers that
  # block later up. Create a normal service replica, keeping the stopped old
  # container for rollback. NEVER use up/start here: it would start both writers.
  project=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$old")
  replicas=1
  number=1
  if [ "$(docker inspect --format '{{index .Config.Labels "com.docker.compose.oneoff"}}' "$old")" = False ]; then
    replicas=2
    number=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.container-number"}}' "$old")
    number=$((number + 1))
  fi
  CANDIDATE="$project-$service-$number"
  if docker container inspect "$CANDIDATE" >/dev/null 2>&1; then
    # Do not let rollback delete a leftover we did not create.
    CANDIDATE=$NEXT
    echo "Existing replacement; inspect the interrupted upgrade before continuing" >&2
    return 1
  fi
  # A temporary empty name permits the extra stopped replica. Clearing depends_on
  # keeps control outside this operation. The next ordinary up may reconcile the
  # temporary name's config hash; it can now do so without a one-off name clash.
  # Explicit -f would replace COMPOSE_FILE, losing production's unpublished
  # ports. Append stdin to the selected files instead of dropping overrides.
  if printf 'services:\n  %s:\n    container_name: !reset null\n    depends_on: !reset {}\n' "$service" |
    COMPOSE_FILE="$COMPOSE_FILE:-" compose create --no-recreate --scale "$service=$replicas" "$service" >/dev/null &&
    docker start "$CANDIDATE" >/dev/null && healthy "$CANDIDATE"; then
    # Keep the stopped original through rename/readiness failures as well.
    docker rename "$old" "$old-previous"
    ROLLBACK=$old-previous
    docker rename "$CANDIDATE" "$old"
    CANDIDATE=$old
    ready "$service"
    ROLLBACK=""
    docker rm "$old-previous" >/dev/null
  else
    return 1
  fi
}

replace connection-backup "$BACKUP"
healthy "$BACKUP"
replace connection-main "$MAIN"
healthy "$MAIN"
# Explicit failback for this upgrade; ordinary recovery never does it.
gateway 'clear table connections'
echo 'Connection upgrade complete; main ready, backup retained'
