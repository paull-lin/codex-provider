#!/usr/bin/env sh
# Compare the request that a native Codex CLI sends with the request produced
# by Claude Code through this adapter.  The source harness remains the source
# of truth for the fake backend; this script only makes a temporary copy so the
# fake can answer Claude Code's Bash tool as well as Codex's exec_command tool.
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"

model=${WIRE_COMPARE_MODEL:-gpt-6-astra}
client_key=${WIRE_COMPARE_CLIENT_KEY:-anthropic-wire-client}
claude_bin=${CLAUDE_BIN:-"$HOME/.local/bin/claude"}
if [ ! -e "$claude_bin" ]; then
  echo "Claude Code binary not found: $claude_bin" >&2
  exit 1
fi
claude_bin=$(readlink -f "$claude_bin")

stamp=$(date -u +%Y%m%dT%H%M%SZ)
work=${WIRE_COMPARE_OUT:-"/tmp/anthropic-wire-$stamp"}
mkdir -p "$work/certs" "$work/logs"
chmod 777 "$work/logs"

suffix=$$
net="anthropic-wire-$suffix"
fake="anthropic-wire-fake-$suffix"
pool="anthropic-wire-pool-$suffix"
adapter="anthropic-wire-adapter-$suffix"
volume="anthropic-wire-data-$suffix"
pool_image=${WIRE_COMPARE_POOL_IMAGE:-codex-pool:anthropic-wire}
adapter_image=${WIRE_COMPARE_ADAPTER_IMAGE:-anthropic-adapter:wire}

cleanup() {
  docker rm -f "$fake" "$adapter" "$pool" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  docker volume rm "$volume" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

echo "Building pool and adapter images"
docker build -t "$pool_image" . >/dev/null
docker build -t "$adapter_image" -f adapters/anthropic/Dockerfile . >/dev/null

# Use the pinned CLI already bundled in the pool image.  This keeps the capture
# offline-friendly while still exercising the release named by upstream/Cargo.toml.
cli_container=$(docker create "$pool_image")
docker cp "$cli_container:/usr/local/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl" "$work/codex"
docker rm "$cli_container" >/dev/null

docker run --rm -v "$work/certs:/c" nicolaka/netshoot:v0.13 sh -ceu '
  cd /c
  openssl req -x509 -newkey rsa:2048 -nodes -keyout ca.key -out ca.pem -days 2 -subj "/CN=anthropic-wire-ca" 2>/dev/null
  cat > san.cnf <<EOF
[req]
distinguished_name=dn
[dn]
subjectAltName=DNS:chatgpt.com,DNS:*.chatgpt.com,DNS:auth.openai.com,DNS:api.openai.com,DNS:*.openai.com
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
EOF
  openssl req -newkey rsa:2048 -nodes -keyout server.key -out server.csr -subj "/CN=chatgpt.com" 2>/dev/null
  openssl x509 -req -in server.csr -CA ca.pem -CAkey ca.key -CAcreateserial -out server.pem -days 2 -extfile san.cnf -extensions dn 2>/dev/null
  chmod 644 ./*
'

docker run --rm -v "$repo_root/scripts/conformance/harness:/h:ro" -v "$work:/w" node:22-alpine \
  node /h/mkauth.mjs current > "$work/auth.json"
cp "$work/auth.json" "$work/auth-expiring.json"
cp scripts/conformance/harness/run-codex.sh "$work/run-codex.sh"

# Work on a temporary copy: Claude Code exposes Bash rather than Codex's
# exec_command, and the stock fake deliberately only selects the latter.
cp scripts/conformance/harness/fake-upstream.mjs "$work/fake-upstream.mjs"
sed -i 's/\["exec_command", "shell_command", "shell", "local_shell"\]/["exec_command", "shell_command", "shell", "local_shell", "Bash", "bash"]/; /if (name === "exec_command") return/ a\\  if (name === "Bash" || name === "bash") return { command: "echo hi" };' "$work/fake-upstream.mjs"
sed -i 's/else if (!hasToolOutput(body) && pickTool(body))/else if (process.env.SIMPLE_TURN !== "1" \&\& !hasToolOutput(body) \&\& pickTool(body))/' "$work/fake-upstream.mjs"

docker network create --internal "$net" >/dev/null
docker volume create "$volume" >/dev/null
docker run --rm -v "$volume:/data" -v "$work:/w:ro" alpine:3.21 sh -ceu "
  mkdir -p /data/accounts/conformance/.codex
  printf '%s' '{\"defaultModel\":\"$model\",\"modelAliases\":{},\"accounts\":[{\"id\":\"conformance\",\"authType\":\"codex_device_auth\",\"enabled\":true,\"inPool\":true,\"priority\":100}]}' > /data/config.json
  cp /w/auth.json /data/accounts/conformance/.codex/auth.json
  chmod 700 /data/accounts/conformance/.codex
  chmod 600 /data/accounts/conformance/.codex/auth.json
  chown -R 999:999 /data"

start_fake() {
  simple=${1:-0}
  : > "$work/logs/capture.jsonl"
  docker rm -f "$fake" >/dev/null 2>&1 || true
  docker run -d --name "$fake" --network "$net" \
    --network-alias chatgpt.com --network-alias auth.openai.com --network-alias api.openai.com \
    -e CAPTURE_LOG=/logs/capture.jsonl -e SIMPLE_TURN="$simple" \
    -v "$work/fake-upstream.mjs:/srv/fake-upstream.mjs:ro" \
    -v "$work/certs:/certs:ro" -v "$work/logs:/logs" node:22-alpine node /srv/fake-upstream.mjs >/dev/null
  sleep 2
}

start_fake 0
admin_hash=$(docker run --rm -e CODEX_POOL_ADMIN_PASSWORD=wire-admin "$pool_image" hash-password)
docker run -d --name "$pool" --network "$net" --network-alias pool \
  -v "$volume:/data" -v "$work/certs:/certs:ro" \
  -e SSL_CERT_FILE=/certs/ca.pem -e CODEX_CA_CERTIFICATE=/certs/ca.pem \
  -e CODEX_POOL_API_KEY="$client_key" -e CODEX_POOL_ADMIN_PASSWORD_HASH="$admin_hash" \
  -e CODEX_POOL_ADDR=0.0.0.0:8317 -e CODEX_POOL_ALLOW_REMOTE_ADMIN=true \
  "$pool_image" >/dev/null
docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$pool" > "$work/pool-ip.txt"
for i in $(seq 1 60); do
  if docker run --rm --network "$net" alpine:3.21 sh -c 'wget -q -O /dev/null --header "Authorization: Bearer '"$client_key"'" http://pool:8317/healthz'; then break; fi
  if [ "$i" = 60 ]; then docker logs "$pool" >&2; exit 1; fi
  sleep 1
done
docker run -d --name "$adapter" --network "$net" --network-alias adapter \
  -e ANTHROPIC_ADAPTER_ADDR=0.0.0.0:8400 -e ANTHROPIC_ADAPTER_POOL_URL=http://pool:8317 \
  "$adapter_image" >/dev/null
for i in $(seq 1 60); do
  if docker run --rm --network "$net" alpine:3.21 wget -q -O /dev/null http://adapter:8400/healthz; then break; fi
  if [ "$i" = 60 ]; then docker logs "$adapter" >&2; exit 1; fi
  sleep 1
done

run_codex() {
  target=$1
  label=$2
  prompt=$3
  simple=$4
  start_fake "$simple"
  docker run --rm --network "$net" -v "$work:/harness:ro" -v "$work/certs:/certs:ro" -v "$work/codex:/codex:ro" \
    -e CONFORMANCE_CLIENT_KEY="$client_key" -e CONFORMANCE_PROMPT="$prompt" -e SIMPLE_TURN="$simple" \
    alpine:3.21 sh /harness/run-codex.sh "$target" "$model" > "$work/$label.codex.log" 2>&1 || true
  cp "$work/logs/capture.jsonl" "$work/$label.jsonl"
}

run_claude() {
  label=$1
  prompt=$2
  simple=$3
  start_fake "$simple"
  docker run --rm --network "$net" -v "$claude_bin:/claude:ro" \
    -e HOME=/tmp/claude-home -e CI=1 -e CLAUDE_CODE_SIMPLE=1 \
    -e ANTHROPIC_BASE_URL=http://adapter:8400 -e ANTHROPIC_API_KEY="$client_key" \
    -e ANTHROPIC_AUTH_TOKEN="$client_key" -e SIMPLE_TURN="$simple" \
    node:22-bookworm-slim /claude --bare --no-session-persistence \
      --permission-mode dontAsk --permission-prompts none \
      --allowedTools Bash --output-format stream-json --verbose -p "$prompt" > "$work/$label.claude.log" 2>&1 || true
  cp "$work/logs/capture.jsonl" "$work/$label.jsonl"
}

run_codex native native-simple "Reply with SIMPLE_OK and do not use tools." 1
run_claude claude-simple "Reply with SIMPLE_OK and do not use tools." 1
run_codex native native-tool "Use a tool to run echo hi, then reply TOOL_OK." 0
run_claude claude-tool "Use Bash to run echo hi, then reply TOOL_OK." 0

docker logs "$pool" > "$work/pool.log" 2>&1 || true
docker logs "$adapter" > "$work/adapter.log" 2>&1 || true

docker run --rm -v "$work:/w:ro" -v "$repo_root/adapters/anthropic/wire-report.mjs:/report.mjs:ro" \
  node:22-alpine node /report.mjs /w > "$work/wire-report.txt"

echo "WIRE_COMPARE_OUT=$work"
cat "$work/wire-report.txt"
