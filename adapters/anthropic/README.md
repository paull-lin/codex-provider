# Anthropic Messages adapter

The roles stack now includes this service behind its gateway, forwarding to
`connection-backup:8317`. See [the production migration runbook](../../deploy/roles/MIGRATION.md)
for the shared public URL, isolated verification and rollback procedure.

A standalone Go sidecar for Claude Code in front of the backup pool connection
node. It shares the repository's Go module, uses only the standard library, and
forwards inference to the pool's streaming `POST /v1/responses`. Adapter code and
tests live here; deployment wiring lives under `deploy/roles`.

## Build and operator wiring

From the repository root:

```sh
docker build -t anthropic-adapter:mvp -f adapters/anthropic/Dockerfile .
```

The image contains a static binary, CA certificates and a non-root runtime user.
It needs no volume. The operator connects it to the backup pool; for example,
on an existing Docker network where `backup-pool` resolves to that pool:

```sh
docker run --rm --network YOUR_EXISTING_NETWORK \
  -p 127.0.0.1:8400:8400 \
  -e ANTHROPIC_ADAPTER_POOL_URL=http://backup-pool:8317 \
  anthropic-adapter:mvp
```

This is an operator example, not an automatic deployment. `127.0.0.1` inside
this container refers to the adapter container, not the host or pool container.

| Environment variable | Default / behavior |
| --- | --- |
| `ANTHROPIC_ADAPTER_ADDR` | `:8400` |
| `ANTHROPIC_ADAPTER_POOL_URL` | `http://127.0.0.1:8317`; a base URL, with `/v1/responses` appended |
| `ANTHROPIC_ADAPTER_MODEL_MAP` | `claude-opus*=gpt-6-astra,claude-sonnet*=gpt-6.1-sol,claude-haiku*=gpt-5.6-luna,default=gpt-6.1-sol` |
| `ANTHROPIC_ADAPTER_ORIGINATOR` | `codex_exec` |
| `ANTHROPIC_ADAPTER_USER_AGENT` | `codex_exec/0.163.0 (Alpine Linux 3.21.7; x86_64) unknown (codex_exec; 0.163.0)` |
| `ANTHROPIC_ADAPTER_REASONING_EFFORT` | unset: follow Claude Code's thinking settings; `low`/`medium`/`high`/`xhigh`: use this effort for every request. The roles stack sets `medium`. |

Model rules match in order, as exact names or trailing-star prefixes. `default=`
or `*=` provides the fallback; absent a fallback, unmatched models use
`gpt-6.1-sol`. Invalid rules fail startup. Originator and User-Agent use the
Codex CLI 0.163 format and are configurable to match the operator's environment.
They do not identify a real native Codex client or reproduce its TLS transport.

## Using Claude Code with the pool

With the roles stack ([deploy/roles](../../deploy/roles/README.md)), the gateway
serves `/v1/messages` on the same public URL as Codex and the dashboard, so
Claude Code needs only that URL and the pool's client key
(`CODEX_POOL_API_KEY`, the same key Codex puts in `/k/<pool-api-key>/`).

1. Write a settings file, for example `~/.claude/settings.codex-pool.json`
   (or put the `env` block in `~/.claude/settings.json` to make it the default):

   ```json
   {
     "env": {
       "ANTHROPIC_BASE_URL": "https://<pool-host>",
       "ANTHROPIC_API_KEY": "<pool-api-key>"
     }
   }
   ```

   Use the bare host: Claude Code appends `/v1/messages` itself, so a base URL
   ending in `/v1` produces `/v1/v1/messages` and fails.

2. Start Claude Code with it and check one turn:

   ```sh
   claude --settings ~/.claude/settings.codex-pool.json -p "Reply with exactly: POOL-OK"
   ```

   Interactive use, tools (Read, Bash, edits), `--continue` and streaming work as
   usual. The request appears in the pool dashboard's totals within about 5s.

3. Pick models with Claude names (`/model opus|sonnet|haiku`); the adapter's
   `ANTHROPIC_ADAPTER_MODEL_MAP` decides which Codex model runs. Reasoning effort
   is fixed by `ANTHROPIC_ADAPTER_REASONING_EFFORT` when set (`medium` in the
   roles stack), otherwise derived from Claude Code's thinking settings.

Claude traffic always goes to `connection-backup`, so a backup upgrade briefly
interrupts it, and Codex failover does not apply to it. Keep the settings file
private: the key grants inference on every pooled account, and HTTPS is required
because it is sent on every request.

### Direct adapter (without the gateway)

For a standalone adapter, point Claude Code at the adapter instead (replace the
URL and placeholder key):

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8400",
    "ANTHROPIC_AUTH_TOKEN": "REPLACE_WITH_POOL_API_KEY",
    "ANTHROPIC_MODEL": "claude-sonnet-4-6",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-4-6",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-4-6",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "claude-haiku-4-5"
  }
}
```

Alternatively, use `ANTHROPIC_API_KEY` in place of `ANTHROPIC_AUTH_TOKEN`.
Configure one credential method. The adapter accepts `x-api-key` (preferred if
both are present) or `Authorization: Bearer` and forwards the selected value as
`Authorization: Bearer`. The pool validates the inference key; the adapter does
not maintain an independent key database. Invalid keys are not accepted merely
because a header is present. The response model remains the requested Claude
model name even though the pool executes the mapped model.

Adapter variables such as `ANTHROPIC_ADAPTER_MODEL_MAP` belong on the adapter
service, not in Claude Code settings. Anthropic-specific headers, stainless
headers, cookies, `x-app`, and the client's User-Agent are not forwarded. The
adapter follows no upstream redirects and logs neither keys nor request bodies.

## MVP contract and limits

- `POST /v1/messages` supports `stream:true` and non-streaming (the default).
  Non-streaming assembles the same translated events into a single message.
  Streaming flushes deltas incrementally and retains only per-item metadata.
- `POST /v1/messages/count_tokens` estimates the entire system/messages/tools
  JSON size at roughly one token per four UTF-8 bytes, rounded up. This is **an
  estimate**, not a tokenizer or an image-token calculator; base64 images can
  substantially overestimate usage. It requires a nonempty credential but runs
  locally without validating it against the pool, performs no inference, and
  does not return pool data.
- `GET /healthz` is unauthenticated local liveness, not a pool readiness check.
- System text becomes a developer message; user text/base64 images and assistant
  text retain order around function calls and results. Tool result text blocks
  are newline-separated; image blocks become Responses input images. Since
  Responses has no `is_error` flag, failed results start with the literal marker
  `[tool_result is_error=true]`. Only custom function tools are supported;
  unsupported blocks/server tools return `invalid_request_error`.
- Tool choice maps `auto`/`any`/`tool`/`none` to `auto`/`required`/named
  function/`none`. Function tools use `strict:false`; parallel calls are enabled.
- `thinking.budget_tokens`: 1–7999 → low, 8000–15999 → medium, 16000+ → high.
  `output_config.effort` low/medium/high overrides the budget; max/xhigh maps to
  high. Adaptive thinking without a budget defaults to medium. Reasoning
  requests include `summary:"auto"`. These are approximate effort mappings.
- Reasoning summary deltas become thinking deltas. `cxr1:` signatures contain
  base64 JSON `{id, encrypted_content}` and restore only those fields on replay.
  They are transport envelopes, not authenticated signatures. Foreign thinking
  and redacted thinking are dropped. Encrypted-only reasoning uses an empty
  thinking block so its replay data survives the next tool turn.
- `max_tokens` is intentionally omitted. The pool's `codexBackendRequestBody`
  removes `max_output_tokens`, and the Codex backend does not support it.
  Consequently this adapter cannot enforce Claude's output-token limit.
- Every inference request uses `stream:true`, `store:false`,
  `include:["reasoning.encrypted_content"]`, and `parallel_tool_calls:true`.
- Session identity is the `x-claude-code-session-id` value, else
  `metadata.user_id`, else a hash of the system prompt and first user message
  (including image data). The same identity goes into `prompt_cache_key` and
  `session-id` / `thread-id` / `x-client-request-id`. Reused user IDs can group
  conversations; compaction or dynamic system prompts can change the fallback
  hash. Supply a conversation-specific header for reliable affinity. Pool
  cache policies can still override the forwarded cache key.
- Terminal usage reports uncached `input_tokens`, `cache_read_input_tokens`,
  and `output_tokens`. Incomplete output due to `max_output_tokens` maps to
  `max_tokens`; calls map to `tool_use`; other completion maps to `end_turn`.
- Lifecycle events and upstream keepalive comments do not immediately commit
  HTTP 200. While waiting, the first heartbeat commits `message_start` and
  `ping` after 15 seconds; subsequent pings recur every 15 seconds. Errors before
  commitment use HTTP JSON (capacity 529, rate 429, auth 401, request 400,
  otherwise 502). Errors after commitment use SSE `error`, with no false
  `message_stop`. Non-streaming failures always use HTTP JSON. Truncated or
  malformed streams are errors. Raw upstream error messages are not echoed.
- Request JSON and individual upstream SSE events are bounded at 32 MiB and
  16 MiB respectively. Non-streaming necessarily buffers the completed output.

Batches, files, citations, web search/server tools, prompt-cache breakpoints,
WebSocket upstream and a dedicated adapter UI are deferred. This
adapter provides wire compatibility; underlying model behavior and reasoning
summaries differ from native Anthropic models. The automated tests use fake
pools and synthetic recorded-style SSE, with no real credentials; the roles
deployment has been validated with real Claude Code (chat, tool use,
`--continue`, streaming) against real accounts.

## Docker-only verification

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  sh -ceu 'gofmt -w adapters/anthropic/*.go; go vet ./...; go test ./...'
docker run --rm -v "$PWD":/src -w /src golang:1.24 \
  go test -race ./adapters/anthropic
docker build -t anthropic-adapter:mvp -f adapters/anthropic/Dockerfile .
```
