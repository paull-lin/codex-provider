# Claude Code / Codex wire comparison

Measured with pinned Codex `rust-v0.163.0` / CLI `0.163.0`, Claude Code `2.1.296`, the fake upstream copied from `scripts/conformance/harness/fake-upstream.mjs`, and fake credentials on a Docker `--internal` network. The capture runs a no-tool turn and a Bash/`exec_command` tool turn. Capture output is written to the path printed by `wire-compare.sh`.

The pool's direct Codex backend path is `/backend-api/codex/responses`. `codexBackendRequestBody` leaves the adapter body unchanged after the adapter supplies a list `input`, `stream:true`, `store:false`, and no unsupported Responses fields. The adapter request is therefore foreign at the HTTP API boundary, but it is already native-shaped for the backend rewrite and the pool rewrites no body fields. It is not a native-endpoint request (`isNativeCodexRequest` is false because the adapter calls `/v1/responses`).

| Wire field | Native Codex CLI | Claude Code → adapter (before) | Claude Code → adapter (after) | Classification |
| --- | --- | --- | --- | --- |
| Transport/path | Responses WebSocket (`GET /backend-api/codex/responses`), then `response.create` messages | HTTP `POST /backend-api/codex/responses`; pool sends zstd | Same HTTP + zstd path | Intentional: the adapter is an HTTP Anthropic Messages sidecar; changing it to a Codex WebSocket would change its public contract. |
| HTTP headers | Native WebSocket order includes `version`, routing hint, session/thread/request IDs, Codex UA/originator, auth/account | Adapter supplies UA/originator and stable session/thread/request IDs; pool adds `version`, routing hint, selected account/auth, and zstd | Unchanged; no Anthropic or client cookies cross the boundary | FORMAT-compatible via the existing adapter allowlist and pool header synthesis. |
| Top-level request keys | `type`/`previous_response_id` occur on WebSocket messages; `stream`, `store`, `include`, `prompt_cache_key`, `text`, `reasoning`, tools | HTTP body had the HTTP subset; omitted `tool_choice` and `text` | Adds `tool_choice:"auto"` and `text:{"verbosity":"low"}`; WebSocket-only fields remain absent | FORMAT gap closed. |
| Input item order/types | developer system, user history, then function call/output items | developer system, user prompt, then `function_call` / `function_call_output` in the same order | Same order/types | FORMAT-compatible; Claude's system prompt and conversation text are inherent content differences. |
| Tool declaration location/shape | Native CLI declares built-in tools top-level (`function`, `namespace`, hosted tools) and may use `additional_tools` for newer experimental tools | Claude tools are top-level Responses `function` tools (`strict:false`) | Same | Inherent content: this release exposes `Bash`, `Edit`, `Read` and their schemas. No `additional_tools` item was present, so moving them to lite/additional format would change the contract. |
| Reasoning/include | Native `reasoning:{effort:"xhigh"}`, `include:["reasoning.encrypted_content"]` | Claude effort maps to `low/medium/high`, with `summary:"auto"`; same include | Same | Inherent model/request policy; encrypted reasoning replay is preserved. |
| `client_metadata` | Codex internal turn/install/guardian metadata and passthrough fields | Omitted | Omitted | Intentional: those values describe a real Codex client and must not be fabricated. Affinity uses `prompt_cache_key` and the three Codex session headers. |
| Model/cache key | Native model and UUID cache key | Claude model is mapped (default `claude-opus* → gpt-6-astra`) and cache key is Claude session ID/hash | Same | Intentional adapter policy; values are stable and route the conversation. |

After the fix, the only remaining differences in both captured turns are the transport choice, Claude's system/user content and tool catalog, mapped model/session identifiers, Claude effort policy, and Codex-only internal metadata. The pool reports no body rewrite for either adapter turn; the adapter unit tests and the Docker capture are the regression checks for that result.

## Hosted web search

Claude Code also makes dedicated search sub-requests to `/v1/messages?beta=true`
with a `web_search_20250305` (or another `web_search_*` version) tool. All such
versions map to the following hosted tool, matching the native CLI 0.163 wire
capture supplied for this change:

```json
{"type":"web_search","external_web_access":true,"search_content_types":["text","image"]}
```

Anthropic `allowed_domains` becomes `filters.allowed_domains`; `user_location`
is passed through when present. `max_uses` and `blocked_domains` are ignored,
so callers cannot rely on those limits. `include` remains exactly
`["reasoning.encrypted_content"]`; the adapter does not request
`web_search_call.action.sources`. A `tool_choice` naming the hosted search tool
maps to `"auto"`; custom functions (even one named `web_search`) retain their
existing mapping. Other server tool types retain the existing 400 error.

The pinned source corroborates the upstream shapes:

- [Codex WebSearchCall and WebSearchAction](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/protocol/src/models.rs):
  the `response.output_item.done` example has
  `{"id":"ws_...","type":"web_search_call","status":"completed","action":{"type":"search","query":"weather: San Francisco, CA"}}`.
  ID, status and action are optional; search accepts optional `query`/`queries`,
  and other actions include `open_page`, `find_in_page` and unknown variants.
- [Codex web search tests](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/core/tests/suite/web_search.rs)
  verify live `external_web_access`, `filters.allowed_domains` and
  approximate `user_location`. The text/image selection above comes from the
  supplied capture; the pinned tests also cover other model configurations.
- [Anthropic web search response and streaming formats](https://platform.claude.com/docs/en/agents-and-tools/tool-use/web-search-tool)
  define `server_tool_use`, `input_json_delta`, full result payloads in
  `content_block_start`, and search usage/error objects.

The shared streaming/non-streaming translator emits a `server_tool_use` with
ID `srvtoolu_` plus the upstream ID (characters outside ASCII letters, digits,
underscore and hyphen become underscores; missing IDs get a local fallback).
Input uses `action.query`, otherwise the first `action.queries` entry, otherwise
`{}`. Unknown action fields are ignored. Each call has one paired
`web_search_tool_result`; failed status produces
`{"type":"web_search_tool_result_error","error_code":"unavailable"}`.

Successful results prefer `action.sources` if supplied (including an empty
list). Otherwise they use URL citations collected from
`response.output_text.annotation.added` and completed message items, including
items on the terminal response. Results are deduplicated by URL, preserving the
first title. Missing URLs are skipped. Without per-call sources, the response's
citation set is shared by all searches: citations have no search-call identity,
so exact attribution is unavailable. Each result has `url`, `title`, empty
`encrypted_content` and null `page_age`; absent sources/citations yield `[]`.

Text deltas are forwarded immediately. Search results wait until completion so
late citations are retained, then use fresh sequential block indices before
`message_delta`. Final usage adds `server_tool_use.web_search_requests` for the
number of observed search-call items, including failures, without changing token
counts. Hosted searches do not require a client tool response or force a
`tool_use` stop reason. Assistant history's `server_tool_use` and
`web_search_tool_result` blocks are dropped; summary text is replayed normally.

The response fixtures are synthetic, grounded in the pinned Codex item model;
optional sources and late citations are defensive compatibility cases. No live
ChatGPT search response was captured or real account used for this change.
The operator's live Claude Code validation remains necessary.
