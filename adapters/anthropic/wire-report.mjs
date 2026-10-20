import fs from "node:fs";
import path from "node:path";

const dir = process.argv[2];
const load = (name) => fs.readFileSync(path.join(dir, name), "utf8")
  .split("\n").filter(Boolean).map((line) => JSON.parse(line));
const requests = (name) => load(name)
  .filter((entry) => entry.kind === "http" || entry.kind === "ws-message")
  .map((entry) => entry.body).filter(Boolean);
const entries = (name) => load(name)
  .filter((entry) => entry.kind === "http" || entry.kind === "ws-message");
// Codex sends an initial generate:false prewarm. Compare the actual turn when
// it exists; keeping the fallback makes failures still show a useful shape.
const first = (name) => requests(name).find((body) => body.generate !== false) || requests(name)[0];
const shape = (body) => body ? {
  top_level: Object.fromEntries(Object.entries(body).map(([key, value]) => [
    key, Array.isArray(value) ? `array(${value.length})` : value === null ? "null" : typeof value,
  ])),
  input_types: (body.input || []).map((item) => item && item.type),
  tools: (body.tools || []).map((tool) => ({
    type: tool.type, name: tool.name, keys: Object.keys(tool).sort(),
  })),
  additional_tools: (body.input || []).filter((item) => item?.type === "additional_tools")
    .map((item) => item.tools || []),
  reasoning: body.reasoning ?? null,
  include: body.include ?? null,
  text: body.text ?? null,
  client_metadata: body.client_metadata ?? null,
  prompt_cache_key: body.prompt_cache_key ?? null,
} : { error: "no upstream request" };
const wire = (name) => entries(name).find((entry) => entry.body?.generate !== false) || entries(name)[0];
const values = [
  ["native-simple", "native-simple.jsonl"],
  ["claude-simple", "claude-simple.jsonl"],
  ["native-tool", "native-tool.jsonl"],
  ["claude-tool", "claude-tool.jsonl"],
];
for (const [label, file] of values) {
  console.log(`\n[${label}]`);
  const entry = wire(file);
  if (entry) {
    console.log(JSON.stringify({
      transport: entry.kind,
      path: entry.path,
      encoding: entry.encoding || null,
      header_order: entry.rawHeaderOrder || null,
      headers: Object.fromEntries(Object.entries(entry.headers || {}).filter(([key]) =>
        ["accept", "content-encoding", "content-type", "originator", "user-agent", "version",
          "x-codex-routing-hint", "session-id", "thread-id", "x-client-request-id",
          "chatgpt-account-id"].includes(key))),
    }, null, 2));
  }
  console.log(JSON.stringify(shape(first(file)), null, 2));
}

const native = first("native-tool.jsonl");
const claude = first("claude-tool.jsonl");
if (native && claude) {
  const keys = new Set([...Object.keys(native), ...Object.keys(claude)]);
  console.log("\n[tool-turn top-level differences]");
  for (const key of keys) {
    if (JSON.stringify(native[key]) !== JSON.stringify(claude[key])) {
      console.log(`${key}: native=${JSON.stringify(native[key])} claude=${JSON.stringify(claude[key])}`);
    }
  }
}
