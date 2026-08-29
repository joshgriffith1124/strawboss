# NOTES — verified against real binaries (not docs)

Findings from running the actual `claude` CLI / opencode that differ from or extend
KICKOFF.md. Add to this file whenever reality disagrees with the docs.

## claude CLI 2.1.251 headless (verified 2026-08-28, real runs; fixtures in `internal/supervisor/testdata/`)

- **Plan-window utilization IS available**, contrary to KICKOFF ("never pretend to know
  % of 5-hour window"). The stream emits `rate_limit_event` lines with
  `rate_limit_info.unifiedWindows.{five_hour,seven_day}.{utilization,resetsAt}`
  (e.g. `0.04` = 4%) around each API call. The dashboard can show a real plan-window
  gauge. KICKOFF's claim was about a separate analytics API; the stream feed makes it moot.
- **Subscription auth confirmed**: with `ANTHROPIC_API_KEY` scrubbed, `system/init` reports
  `"apiKeySource":"none"` and turns complete fine. The driver asserts nothing yet, but the
  init event exposes the field — the TUI should alarm if it's ever not `none`.
- `--output-format stream-json` requires `--verbose` in `-p` mode (documented, confirmed).
- `--include-partial-messages` exists and works with `-p --resume`; partials arrive as
  `stream_event` lines wrapping raw API events (`message_start`, `content_block_delta`
  with `text_delta`/`input_json_delta`, `message_delta` carrying stop_reason + usage, …).
- **Event sequence observed** (turn with one tool call):
  `rate_limit_event?` → `system/init` → `system/status ("requesting")` → stream_events →
  `assistant` (complete msg: tool_use block) → `rate_limit_event` → `user` (tool_result) →
  `system/status` → stream_events → `assistant` (text) → `result`.
  Complete `assistant`/`user` lines arrive interleaved with the partials, so a parser can
  ignore stream_events entirely and lose nothing but live typing.
- `user` tool_result lines: `message.content[].content` is a string OR a block list; Bash
  results also carry a top-level `tool_use_result` with `{stdout, stderr, interrupted}`.
  `message.content` itself can also be a plain string (no tool results) — handle both.
- `result` line: `total_cost_usd` is the **notional** API value (marginal real cost $0.00 on
  subscription); `usage` has full cache split; `modelUsage` breaks out per-model (includes a
  small `claude-haiku-4-5` helper the CLI uses internally); also `num_turns`,
  `permission_denials`, `duration_api_ms`.
- **Supervisor context is inherited from the environment**: the spawned CLI loads the cwd's
  CLAUDE.md, user memory, and any user-configured MCP servers (our first spike turn burned
  ~17.8k input tokens mostly on that). For a lean supervisor, pick its cwd deliberately and
  consider `--strict-mcp-config` / a minimal system prompt later.
- Resumed turns re-emit `system/init` with the same `session_id` each invocation.
- SIGINT/SIGTERM semantics are exercised in driver tests against a fake binary only so far;
  real-binary interrupt behavior still to be spot-checked during M5.

## Actual worker topology (differs from KICKOFF's picture; verified 2026-08-28)

- **opencode runs locally on this box** (npm global under nvm, v1.18.25), not on the GX10s.
  The GX10 is a bare sglang OpenAI-compatible endpoint (`http://gx10-52e4.local:8000/v1`)
  behind opencode's `spark-a` provider in `~/.config/opencode/opencode.json`.
- Consequently `models.toml` `endpoint` = the **opencode server URL** (local), and `model` =
  opencode's `provider/model` ref (e.g. `spark-a/qwen3.8-27b`). GX10 endpoints stay
  opencode's concern.
- Currently only `qwen3.8-27b` is loaded server-side (config also lists `qwen3.8-flash-next`,
  `deepseek-v4-flash`; `/v1/models` shows one). 262k context, sglang.

## opencode 1.18.25 serve API (verified live; fixtures in `internal/harness/opencode/testdata/`)

- Session storage is **SQLite** (`~/.local/share/opencode/opencode.db`) now, not JSON files —
  KICKOFF's "session storage fallback" doesn't apply; the server API is the only route.
- `opencode serve` defaults to a **random port** (`--port 0`); pass `--port`. Unsecured
  without `OPENCODE_SERVER_PASSWORD` (fine on localhost).
- **v2 `POST /api/session/{id}/prompt` only queues input — it does not start a run**
  (`/api/session/{id}/wait` then 503s). The path that actually runs fire-and-forget is v1
  `POST /session/{id}/prompt_async` with `{"model":{"providerID","modelID"},"parts":[{"type":"text","text":...}]}` → 204.
- `GET /session/status` returns only non-idle sessions (`{}` when everything is idle);
  absence means idle. Status types seen: `busy`, `retry`, `idle`.
- **`GET /event` is scoped to sessions rooted in the server's own cwd.** Workers running in
  other directories only appear on `GET /global/event`, where each SSE event is wrapped as
  `{"directory","project","payload":<event>}`. Event types consumed: `message.part.delta`
  (`field`:"text"/"reasoning" + `delta`), `message.part.updated` (full tool part with
  state pending→running→completed/error), `session.status`, `session.idle`.
- **An assistant message record exists from the moment generation starts** (reasoning parts
  stream in while `info.time.completed` is 0). "Turn finished" = session idle AND last
  assistant message has `time.completed != 0`. Also: `prompt_async` admits before the
  session shows busy, so idle-at-first-poll ≠ finished (race hit in practice; harness
  handles both).
- Cumulative session tokens (incl. cache read/write) at v2 `GET /api/session/{id}`;
  per-message tokens on each assistant message. No tok/s from the API — compute from
  output-token deltas over time if wanted.

## Full chain verified (2026-08-28, M3)

`claude -p --allowedTools 'Bash(<path>/strawboss delegate:*)'` → delegate → opencode →
GX10 worker → file on disk → 2-line terse result in supervisor context → supervisor
verified and summarized. No permission prompts. Notes:

- The path-prefixed allowedTools pattern (`Bash(/abs/path/strawboss delegate:*)`) works.
- The supervisor also ran a bare `cat` that was NOT in allowedTools and it executed —
  headless default permission mode appears to allow read-only commands. Don't rely on
  allowedTools as a sandbox; it's prompt-avoidance, not confinement.
- **Compound shell commands don't pass the allowlist** (verified in `dontAsk` mode):
  `delegate … & delegate … & wait` is denied even with `Bash(wait)` allowed — the
  matcher doesn't decompose `&` chains into individually-allowed parts. That's why
  `delegate` accepts repeated `--task` flags: parallelism lives inside one allowed
  command. Also: a resumed session REMEMBERS past denials and avoids the denied form
  even after the allowlist changes — prompt-engineering fixes need `--new`.

## M5 integration notes (2026-08-28)

- Full live loop verified in the TUI: prompt → supervisor turn → one delegate call
  with two --task flags → two workers running simultaneously on the GX10
  ("Workers · 2 active"), files landing on disk, terse results back, session
  persisted and resumed across strawboss restarts.
- **tok/s and mid-run worker tokens only move when a worker message completes** —
  opencode's session token counters update per completed assistant message, so
  single-message tasks (seconds long) show `idle`/0 until done. Multi-step tasks
  (the real workload) update every few seconds. Known cosmetic limitation.
- strawboss now spawns and babysits `opencode serve` itself for any localhost
  endpoint in models.toml that fails its health check (child process, log at
  `~/.strawboss/opencode-serve.log`, killed on exit). Remote endpoints are only
  reported (`endpoint unreachable`), never managed.

## The hung-delegation incident (2026-08-28, first real task)

Two stacked failures produced an indefinite delegate hang; both handled now.

1. **`GET /session/status` is project-scoped like `/event`**: without
   `?directory=<worker dir>` it returns `{}` for sessions rooted outside the
   server's own cwd, so delegate never saw its worker go busy. Every status query
   now carries the worker's directory. (M3/M5 tests passed by luck: their workers'
   final messages completed, satisfying the fallback exit condition.)
2. **opencode can end a run silently mid-message**: the sglang server on the GX10
   restarted mid-generation (model `created` timestamp jumped past the worker's
   death), the in-flight request died, and opencode left the session idle with an
   INCOMPLETE final assistant message and `error: null`. Waiting for a completed
   message therefore never returns. `Result` now declares a worker dead when it
   goes idle with an incomplete message (debounced), or when its session record
   goes stale (`StallAfter`, default 45s) — reported as a failed worker with the
   partial transcript at the log path. `finishedStatus` also no longer classifies
   an incomplete message as done (that briefly made the TUI mark live workers
   "recovered").

Also fixed from the same run: the logs tab was spewing every streamed worker
delta as its own fragment line (now only discrete events are logged; deltas
coalesce in the detail pane) and worker output ANSI/control characters are
stripped before rendering.

## Persistent supervisor via --input-format stream-json (verified 2026-08-28)

KICKOFF assumed `--input-format stream-json` might not exist — it DOES in CLI
2.1.251 ("realtime streaming input"), which obsoletes the one-process-per-turn
pattern for the TUI:

- One `claude -p --input-format stream-json --output-format stream-json` process
  stays alive across turns; user messages are stdin JSON lines
  (`{"type":"user","message":{"role":"user","content":[{"type":"text","text":…}]}}`).
- **Messages sent MID-TURN are delivered into the running turn** (verified: the
  model acknowledged and honored a mid-turn instruction) — the supervisor is
  never deaf while workers run. This was the motivation: with per-turn
  processes, input typed during a turn just queued silently.
- `system/init` is emitted only after the FIRST message, not at spawn — keeping
  the process warm is free.
- One `result` event per turn (num_turns cumulative); the process does not exit
  between turns.
- SIGINT terminates the whole process (unlike interactive esc). Interrupt is
  therefore: kill → respawn with --resume on the next prompt — identical to
  crash recovery. Closing stdin ends the process gracefully.
- New stream event: `system/thinking_tokens` `{estimated_tokens,
  estimated_tokens_delta}`, emitted periodically while reasoning — now feeds
  the status line ("thinking… ~82 tok") instead of spamming logs as unparsed.
- `strawboss chat` (console spike) still uses the per-turn --resume driver.

## The retry-loop incident (2026-08-28): reasoning exhaustion

The supervisor kept re-delegating the same big task. Cause: qwen3.8-27b (a
reasoning model) burned its ENTIRE output budget thinking — one worker produced
49,778 chars of reasoning, zero text, zero tool calls, output tokens pegged at
exactly the 16384 `limit.output` from opencode.json — and opencode recorded a
CLEAN completion (no error, no finish reason). The terse result read "done ·
(empty reply)", the file never existed, so the supervisor retried the same task
into the same wall, forever.

Handled three ways:
- A completed message that is pure reasoning (no text, no tool parts) is now a
  FAILED result with advice ("output budget exhausted thinking — split the task,
  don't retry it as-is"), and delegate exits 1.
- The system prompt tells the supervisor workers have a ~16k output budget
  shared with reasoning: scope tasks to ~200-line deliverables, split big files.
- `models.toml` entries accept `variant` (opencode ModelRef variant, e.g. a
  non-thinking mode if the provider defines one), passed through prompt_async.

Also worth knowing: `limit.output` in ~/.config/opencode/opencode.json is the
lever for the budget itself (currently 16384 for spark-a models).

## DeepSeek Harness (dsh) 0.1.1-rc.2 as a second worker harness (verified 2026-08-29)

Verified live against the installed npm global `@deepseek-ai/dsh` and a fake
OpenAI-compatible endpoint; fixtures in `internal/harness/dshacp/testdata/`.

- **The worker entry point is the `dsh-acp-demo` bin** (an [Agent Client
  Protocol](https://agentclientprotocol.com) JSON-RPC server on stdio), run
  directly with `--config <cordis.yml>` — NOT via the `dsh` profile launcher.
  ndjson framing (no Content-Length headers). Sequence: `initialize` →
  `session/new {cwd}` → `session/prompt` (BLOCKS until the turn quiesces,
  returns `{stopReason: end_turn|cancelled}`). Committed assistant text
  arrives as `session/update` `agent_message_chunk` notifications — a
  natural terse summary. Reasoning, tool activity, and usage deliberately
  stay OFF the ACP wire.
- **Plugin specifiers resolve relative to the config file's directory**, so
  the cordis.yml must live inside the dsh profile tree; strawboss writes
  `~/.dsh/profiles/acp/strawboss.cordis.yml` (env-parameterized, never
  touches the profile's own files). The acp profile must hold the acp-demo
  packages: `dsh plugin --profile acp add @deepseek-ai/dsh-acp-demo
  @deepseek-ai/dsh-acp @deepseek-ai/dsh-agent-spine-demo` plus the leaf
  plugins (llm-deepseek, sandbox-*, bash-sandbox, subprocess-local,
  user-approval, system-prompt, fs-*, tool-fs). pnpm `autoInstallPeers` is
  false: peers do NOT come in automatically (the missing-peers state is
  exactly where the 2026-08-28 session died mid-setup).
- **Observability = the session JSONL**: with `persistenceCompression:
  none`, every event lands in
  `<persistenceRoot>/<mangled-cwd>/<sessionId>/session.jsonl` live:
  `assistant/chunk` (`text-delta`/`reasoning`-typed blocks,
  `tool-call-delta`, `usage {inputTokens,outputTokens}`, `finish`),
  `tool/call`, `tool/result`, `turn/start`, `turn/end {reason}`. Find the
  file by globbing for the session id — don't reimplement the cwd mangling.
- **`dsh-llm-deepseek` takes a `baseURL` override** (chat-completions +
  SSE), so workers can point straight at the GX10 sglang endpoint; model
  ids pass through unchanged. `apiKeyEnv` names the env var. Requests carry
  `reasoning_effort` and `thinking:{type}` — sglang tolerance for those
  fields is UNVERIFIED (GX10 was down); revisit at live integration.
- **`dsh-user-approval` `policy: never` means never ASK = auto-REJECT**,
  not auto-allow. Unattended workers therefore need `sandbox-policy`
  `mode: danger-full-access` (nothing asks) — the runtime-context message
  confirms: "actions that require approval are rejected automatically".
- The bash tool schema requires `description` alongside `command`; a
  malformed tool call yields a clean `Error: invalid arguments` tool result
  and the loop continues.
- Boot diagnostics go to stderr (stdout is the wire); stdin EOF is
  graceful shutdown. Kill from outside = SIGTERM the bin's PID.

## dsh tool-transport modes (verified 2026-08-29, real bin + fake LLM)

The acp-demo app config `tools.mode` selects the model-facing tool
transport; strawboss exposes it per model as `tools_mode` in models.toml
(→ `STRAWBOSS_DSH_TOOLS_MODE`, read by the generated cordis.yml):

- **native** (default): ordinary wire tools (`bash, read, write, edit,
  skill, job_*, *_goal` — 11 in rc.2), system prompt ~2.3KB.
- **code**: the wire carries ONE `run_code` tool; the model writes
  TypeScript executed in a fresh worker thread per run
  (`dsh-code-runtime-worker-thread`, bash-equivalent trust posture,
  compute/wall/heap/output budgets). The generated TS SDK inflates the
  system prompt to ~15.6KB (≈4k tokens per request) — significant for
  small local models, hence opt-in, not default.
- **both**: native tools + `run_code`.

code/both require the `dsh-code-runtime-worker-thread` plugin mounted;
the strawboss composition mounts it unconditionally — verified inert
under native mode (same tool list and prompt size as without it). It is
present in the shared profile packages; no extra install.

## dsh × sglang live integration (verified 2026-08-29, qwen3.8-27b on the GX10)

First real dsh worker ran end-to-end (delegate → dsh-acp-demo → sglang →
file on disk → terse result, 5s). Three obstacles, all handled:

1. **`reasoning_effort` vocabularies clash**: dsh-llm-deepseek knows
   off|low|high|max and sends `high` by default; this sglang build accepts
   xhigh|medium|low (400 otherwise). Only `low` is in both. The worker
   composition now defaults `reasoningEffort` to `off`
   (`STRAWBOSS_DSH_REASONING` overrides), which keeps the field off the
   wire entirely; sglang tolerates the accompanying
   `thinking: {type: disabled}` (and ignores it — qwen still reasons at
   the server's default effort).
2. **sglang streams explicit nulls in tool_calls deltas** (`"id": null`,
   `"function": {"name": null, ...}` on every chunk after the first) where
   DeepSeek's API omits the keys. dsh's translator merges with
   `!== undefined`, so the nulls overwrite the real name/id → every tool
   call fails as `unknown tool ""`. Same merge on dsh master — reported
   nowhere yet. Workaround: the dshacp harness routes each worker's LLM
   traffic through a local reverse proxy (proxy.go) that deletes those
   null fields from SSE chunks and passes everything else through.
3. **Go resolves the GX10 hostname to dead IPv6 only**: the WSL DNS path
   hands Go's pure resolver ten stale AAAA records and no A (glibc callers
   like curl get the working IPv4 — and this Go toolchain lacks cgo
   resolver support, so GODEBUG=netdns=cgo is a no-op). The proxy and the
   endpoint probe now dial IPv4-first over the full address list, but on
   this box the name still needs a WSL /etc/hosts line
   (`192.168.1.94 gx10-52e4.attlocal.net`) until the DNS side serves an A
   record. GX10 endpoint hostname is now gx10-52e4.attlocal.net (the
   .local name died with the IP change).

## dsh parallel workers vs session-query.db (2026-08-29, live TUI run)

Concurrent dsh workers sharing one persistence root die at boot with
`ERR_SQLITE_ERROR: database is locked`: the acp app derives a
`session-query.db` (SQLite, single-writer) at the persistence root, and
every worker process opens it (3 of 4 parallel workers died; opencode was
immune — one server process). Fixed by giving each worker its own
persistence subtree `<stateDir>/dsh-sessions/w-<pid>-<nano36>/`;
FindSessionLog globs both depths. Parallel dsh delegation works after
this — no need to run dsh tasks sequentially.

## OpenClaw remote notify + two-way control (verified 2026-08-29, OpenClaw 2026.7.1-2)

Verified against the live gateway (ws://127.0.0.1:18789) and the real
Discord channel:

- Outbound: `openclaw message send --channel discord --target channel:<id>
  --message … --json` returns the platform message id. Target ids come
  from `openclaw sessions list --json` (key
  `agent:main:discord:channel:<id>`) or `openclaw directory`.
- Inbound: `openclaw message read --channel discord --target … --json
  [--after <id>] --limit N` returns Discord-shaped messages (newest
  first): `id` (snowflake, strictly increasing — a clean poll cursor),
  `content`, `author.bot`/`author.username`. Filtering on `author.bot`
  excludes both our own notifications and the OpenClaw agent's replies,
  so the loop cannot feed itself.
- strawboss polls (default 5s) instead of receiving webhooks — no server
  in strawboss (KICKOFF stack rule); the exec-per-poll cost is a node
  startup, fine at that cadence. Channel history at startup is baseline,
  never treated as commands.
- Two-way semantics: a human message becomes a supervisor prompt (tagged
  "[via discord]", injected mid-turn like typed input — user input, not
  observability, so invariant 2 is untouched), the message is echoed
  into the chat tab, and completed supervisor replies relay back to the
  channel until local TUI input resumes.
- Note: the OpenClaw agent ALSO sees messages in that channel and may
  reply on its own — that's OpenClaw config territory (channel routing /
  allowlists), not something strawboss can suppress.

## dsh vs opencode speed (verified 2026-08-29, controlled comparison)

Perceived dsh slowness investigated; dsh is NOT slower per worker.
Identical task (write+run fib.py), same model, same box: qwen-dsh 20.0s
vs qwen-coder/opencode 30.1s — dsh won (smaller system prompt, lean
loop; node boot ≈ 0.4s of the 20s; sglang prompt cache works across
steps: cacheReadTokens grow per request). Raw generation throughput was
equivalent all day (55–78 out-tok/s both harnesses). The slowness was:

1. **Output budget mismatch**: the dsh template capped maxTokens at
   16384 while opencode.json limit.output had been raised to 49152 —
   big tasks ground ~290s to an output-budget FAILURE on dsh (w40, w41:
   out≈16.4k, failed) where opencode finished (w32: 17.8k output, done).
   Fixed: models.toml `max_tokens` (dsh only), default 49152 for parity.
2. The since-fixed session-query.db lock forcing sequential dsh runs.

Also fixed for comparability: dsh usage now counts cacheReadTokens as
input, matching opencode's session-total accounting (dsh "in" numbers
looked misleadingly tiny before).

## Per-project session scoping (2026-08-29, live incident)

`strawboss` launched in a NEW project directory resumed the OLD
project's supervisor — full old context, started planning Farkle
features from an unrelated repo. Cause: the supervisor-session and run
pointers were single global files under ~/.strawboss. They are now
scoped per working directory (`~/.strawboss/projects/<hash>/`, with a
`dir` file naming the path); the legacy global files are ignored. One
transitional effect: each project's first launch after this change
starts a fresh session (`claude -r <old-session-id>` still works
manually if an old conversation matters — note the Claude CLI happily
resumes a session id from a different cwd, which is what made the bug
bite instead of erroring).

Also from the same screenshot: in dontAsk mode a bare `ls` IS denied
(contradicting the earlier M3-era observation that read-only commands
slipped through) — the default allowedTools now include Glob so the
supervisor has a sanctioned way to look around a directory.
