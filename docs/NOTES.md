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
