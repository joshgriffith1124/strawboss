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
