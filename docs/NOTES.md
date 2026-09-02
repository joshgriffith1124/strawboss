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
  The GX10 is a bare sglang OpenAI-compatible endpoint (`http://<gx10-host>:8000/v1`)
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
   this box the name still needs a WSL /etc/hosts line mapping the GX10
   hostname to its IPv4 until the DNS side serves an A record. (The
   box's mDNS .local name also died with an IP change — the router's
   DNS name is the stable one.)

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

## Loop detection, three layers (2026-08-30)

Small local models loop instead of concluding — the reasoning-exhaustion
guard was one instance of a general problem (credit where due: rexyMCP's
small-model hardening list prompted this). Three independent layers now:

1. **Advisory, in the worker's own context (dsh)**: the composition
   mounts `@deepseek-ai/dsh-repeat-tool-reminder` (thresholds 3/5/8) —
   escalating in-context nudges when the model repeats the same tool
   call with identical canonical arguments. Never blocks a call.
2. **Hard abort at the harness**: both harnesses watch for consecutive
   identical tool calls and abort past a threshold (dsh 10, above the
   last advisory; opencode 6, no advisory layer there), returning a
   failed terse result with "needs a different approach" advice —
   minutes of timeout burn become a fast failure. dsh watches the
   session-log tail live; opencode scans the transcript every ~10s of
   polling (identity = tool name + title, the best the API exposes).
3. **Delegation-loop guard in delegate**: a task byte-identical to one
   that already FAILED twice this run (same model) is refused before
   spawning — "change the approach, split it, or ask the user" — the
   structural end of the supervisor retry loop.

## dontAsk denials name the whole tool, not the pattern (2026-08-30, live incident)

A supervisor (poe-upgrade-advisor project) ran one `git status && git log
&& git diff` chain — correctly auto-denied, git isn't allowlisted — and
the denial text was "Permission to use Bash has been denied because
Claude Code is running in don't ask mode". It names the *tool*, never
the unmatched pattern, so the model generalized to "Bash is off
entirely" and stopped attempting delegation for the rest of the session
even though `Bash(<exe> delegate:*)` was allowlisted and would have run.
One denied git command silently defeated the whole topology.

Fixes: the built-in system prompt now spells out that Bash covers only
the delegate command and that a denial of anything else never means
delegation is blocked; and config `allowed_tools` are appended to the
baseline (delegate + Read/Edit/Write/Glob) instead of replacing it, so
users can grant e.g. `Bash(git status:*)` without being able to lose the
delegate pattern. Recovery for an already-poisoned session: tell the
supervisor in chat that only non-delegate Bash is denied.

## Resume is a 500k-token ambush without a context gauge (2026-08-30, live incident)

Starting the TUI resumes the project's last supervisor session by design
— but nothing showed how big that session had grown. A fresh strawboss
launch resumed an old conversation and the first prompt instantly
re-read ~500k tokens of cache for possibly stale history; every later
call re-reads it all again. The only reset was quitting and relaunching
with `--new`.

Now: the supervisor's context footprint (each API call's full prompt =
input + cache read + cache write from the per-message usage) is tracked
live, persisted in the per-run ledger (`ctx` in sup-usage-<run>.json),
and seeded back at startup — so a resumed session shows its footprint
BEFORE the first prompt burns. Crossing 100k advises a fresh session
once (chat note + toast); the chat tokens panel and supervisor detail
panel (mockup's `context 168k/200k` line) show it always. `/new` in
chat — or `n` on the dashboard/logs tabs and in the session picker —
starts a fresh session in-TUI: stream ends (old session stays in the
picker), session pointer clears, fresh run id, budget stop lifts.

Follow-up bug from the first live use: rotating the run has to repoint
the driver's STRAWBOSS_RUN env too, not just the orchestrator/persisted
ids. The delegate stamps registry events with the env value inherited
through the supervisor — left stale after `/new`, every new worker
landed in the OLD run and the watcher (scoped to the new one) silently
dropped it: workers ran to completion while the table said "no workers
yet". Session switch had the same latent bug. Both now update the env
while the stream is down (`Driver.SetEnvVar`), so the next spawn stamps
the current run.

## "unreachable" dsh models: WSL2 DNS proxy + router EDNS quirk (2026-08-30, live incident)

Both dsh entries showed "unreachable" while `curl` reached the same
sglang endpoint fine (HTTP 200, model listed). Not an endpoint problem —
a name-resolution split between Go and libc, surfaced after a WSL
restart:

- The models panel probe (`llmModels`) and the dsh worker proxy both
  resolve through Go's resolver (`net.DefaultResolver` /
  `dshacp.Transport`), which always sends EDNS0 queries.
- The home gateway's DNS (authoritative for the router-assigned LAN
  hostnames, reached via WSL2's 10.255.255.254 proxy) answers EDNS
  queries with the records in the wrong order: the echoed OPT record
  comes FIRST, before the answer, while the counts still say answer=1.
  A section-ordered parser (Go's) reads the OPT as the whole answer
  section, demotes the real A record to authority, finds no usable
  answers → "no such host". Go then tries the search-suffixed name and
  gets REFUSED.
- Plain queries without EDNS come back well-formed — which is why libc
  (curl, python, getent, and Node/opencode, hence qwen-coder staying
  healthy on the same box) resolves the very same name fine.

So the panel was truthful in the way that matters: dsh delegations
would fail identically, since the worker proxy shares the dialer.

Workarounds, most robust first: pin the LAN host in `/etc/hosts` (Go's
resolver honors files before dns), or use the LAN IP in models.toml.
A code-level fallback (retry the lookup with a plain no-EDNS query when
LookupIPAddr fails) would make strawboss immune to this router class.

## Steal-list wave 1: worktreeinclude, escalation, repo map, chat scroll (2026-08-30)

Three adoptions from the ecosystem survey, picked for fit with the
invariants, plus a UX fix:

- **`.worktreeinclude`** (ccmanager convention): gitignored paths listed
  there (env files, local certs) are copied into each worker worktree at
  creation — kills "works in the main checkout, fails in the worktree".
  Escaping patterns refuse; tracked files are never overwritten;
  symlinks are skipped.
- **Cheap-first escalation** (RA.Aid): a failed worker re-dispatches its
  task ONCE to the next config in models.toml file order — the ladder IS
  the preference order — inside the same delegate call, so the
  supervisor reads a single terse result telling the whole story (both
  attempts appear in the registry/TUI). Skipped with --worktree (the
  second attempt would share the first's partial state); --escalate=false
  opts out. The system prompt tells the supervisor not to hand-retry.
- **Repo map for workers** (Aider): `git ls-files` + regex-extracted
  top-level symbols (Go/Python/JS/TS/Rust/Ruby), capped at ~6KB,
  prepended to every worker prompt (map first, task last). Regex over
  tree-sitter on purpose — measure first-pass success before buying real
  parsing. The registry records the BARE task: loop-guard identity and
  TUI display stay clean. --repomap=false opts out; retry from the TUI
  gets the same map.
- **Chat scrolling**: PgUp/PgDn walk into history (long supervisor
  replies used to overflow with no way back); a "N lines below"
  indicator row shows while scrolled; sending a message snaps back.

## A second host: Apple Silicon + a TLS-inspecting network (2026-09-01, live setup)

Standing up strawboss on a work MacBook (M3) over ssh, per the README's
"Working on a remote machine". Everything below was hit for real.

- **Cross-compiling the binary is enough** — no Go on the target.
  `GOOS=darwin GOARCH=arm64 go build` from Linux emits a Mach-O arm64
  binary that the Go linker **ad-hoc code-signs itself**
  (`LC_CODE_SIGNATURE` is present in the cross-built output), so Apple
  Silicon runs it without `codesign -s -`. `scp` sets no quarantine
  xattr, so Gatekeeper stays quiet. Check the artifact with `file`
  before shipping: `zsh: exec format error` on the far side just means
  an ELF got copied to a Mac.
- **Node is the only thing that breaks under TLS inspection.** macOS
  `curl` and Go's darwin `crypto/x509` both use the system keychain, so
  the `curl | bash` installers and strawboss itself are fine; Node ships
  its own CA bundle, so npm/pnpm, opencode, dsh, and the `claude` CLI
  all fail until the corporate root is added.
  - `security find-certificate -c <name>` matches the common name
    **exactly**, not as a substring — it finds nothing for a partial
    name even when the cert is right there. Dump the keychain with
    `-a -p`, split on `BEGIN CERTIFICATE`, and filter on
    `openssl x509 -noout -subject`.
  - `NODE_EXTRA_CA_CERTS` **appends** to Node's bundled roots (right
    thing); `npm config set cafile` **replaces** them. pnpm keeps its own
    `cafile` config separate from npm's.
  - The var must be exported from the shell rc *before* strawboss
    launches: the supervisor `claude` subprocess inherits the
    environment, so an interactive-only export leaves the supervisor
    failing TLS while the setup commands all looked fine.
- **Pin dsh to one prerelease across both layers.** The global `dsh`
  package and every profile package must be the same version (here
  `0.1.1-rc.2`); the leaf plugins and the `dsh-base` bundle resolve out
  of the *global* install's own node_modules, while the acp profile
  holds only the eight `dsh-acp*`/leaf packages. `dsh plugin --profile
  acp add <pkg>` forwards verbatim to `pnpm add` in the profile dir, so
  an unversioned spec resolves the `latest` dist-tag — wrong or absent
  for a prerelease-only package. Pass explicit `@<version>` specs.
- **Laptops sleep.** Idle sleep and lid close both suspend the
  supervisor and any live workers. `caffeinate -i` covers idle only;
  lid-close needs `caffeinate -d` or a power-settings change. macOS also
  has no preinstalled tmux, so the README's detach story needs Homebrew
  on a clean machine.

## Impossible worker tok/s: a first sample counted as a delta (2026-09-01)

The models panel showed ~1700 tok/s for a local worker — an order of
magnitude past what the endpoint can do. Not a display bug: the poll loop
accumulated `out - lastOut[worker]` into the per-model delta, and a
worker seen for the FIRST time has no `lastOut` entry, so its whole
cumulative output counted as one 2s interval's work. A dsh tailer
attaching to a session already in flight (TUI restart into a resumed run,
a recovered worker) lands exactly there: ~3.4k accumulated tokens ÷ 2s ≈
1700.

Now a `rateTracker` seeds a worker's baseline on first sight and credits
nothing, returns zero when the count goes backwards (restarted worker /
reset session), and drops baselines for workers that are no longer open
so a returning id re-seeds instead of spiking. The divisor is also real
elapsed time now, not the nominal 2s constant — a slow polling pass was
dividing a longer window's tokens by 2s and overstating the rate.

The per-worker rate in the UI model was already correct: it skips its
first sample via a zero-time guard. Only the panel's per-model
aggregation had the hole.

Unrelated flake found while verifying: `TestSessionHistoryAndSwitch`
stopped draining at the replayed worker event, so if the switch message
from the other producer hadn't landed yet the assertion failed. It waits
for both now.

## Box-drawing borders render as `?` in some remote terminals (2026-09-01)

Running the TUI inside zellij over ssh, the panel borders came back as
runs of `?` while everything else drew fine. Ruled out on our side:
`panel()` (internal/ui/styles.go) is the ONLY runtime source of `─`, it
builds rules with `strings.Repeat` on a whole rune, and `clipTo` trims by
`[]rune` — nothing byte-slices a multi-byte sequence.

It is the terminal, and the tell is *which* glyphs survive: `·`, `▰`,
`✻` and the block elements rendered correctly while only the
box-drawing characters were substituted. That rules out a non-UTF-8
locale (which would break everything non-ASCII) and points at the DEC
line-drawing set specifically. `?` rather than `▯`/`�` also means
substitution, not a missing font glyph.

Reproduce without strawboss in the same pane:

    printf 'horiz:───── vert:│ corners:┌┐└┘ dot:· block:▰\n'

An ASCII fallback border set was considered and declined — the borders
come from one function, so it stays cheap to add if a terminal that
can't be fixed turns up.

## The lost crash: fatal errors bypass recover, and stderr was ephemeral (2026-09-02, live incident)

A full crash in the lineforge run. The traceback existed only in the
terminal's scrollback and came back mangled by the multiplexer, so the
`fatal error:`/`panic:` line — the one that names the cause — was gone.

What the surviving fragment still proves: the goroutine headers carried
`gp=… m=nil` and every frame carried `fp=/sp=/pc=`. That detail is
GOTRACEBACK=system output, which the runtime forces for a **fatal error**
(`runtime.throw`) and not for an ordinary panic. So the cause is in the
class of concurrent map writes / out of memory / deadlock — and
**`recover()` would not have caught it**. A recover-based crash handler,
the obvious fix, would have changed nothing.

Capture therefore has to be at the file descriptor: the runtime writes
tracebacks straight to fd 2, so reassigning `os.Stderr` misses them.
`captureStderr` now dup2/dup3s a `<state-dir>/crash-<pid>.log` onto fd 2
before the alt screen goes up, and removes the file on a clean exit.
Losing stderr costs nothing — anything written there during an alt-screen
TUI corrupts the display rather than being read. `syscall.Dup2` is absent
on linux/arm64, so linux uses `Dup3(…, 0)` and darwin `Dup2`.

Timeline reconstructed from state files alone (all that survived):
`projects/<hash>/dir` named the run, the registry's last event was
`w165 failed — aborted: terminated signal received` at 13:14:33 (cleanup
killing live workers on the way down), and w165's session log showed a
runaway: 11 minutes, 45 steps, 6509 events, 1.4MB, the only worker still
running for the final 8 minutes of a 167-worker run.

Audited and cleared while hunting it: chat scroll bounds (`logH` clamps
to ≥3, so the window cannot invert), every space-padding
`strings.Repeat` (all clamped), the sparkline (clamped both ends,
NaN-safe), per-worker event buffers (capped at 200), the `Listen` re-arm
(a feedBatch re-arms exactly once), and lock coverage on all eleven
Orchestrator maps. One real hole did turn up and is fixed: the token
bar's `strings.Repeat("▰", …)` counts were unclamped, so a negative
share would panic — now `barCells` clamps into the bar.

The race detector could not be run: it needs cgo and this box has no C
compiler at all (no gcc, cc, or clang). Worth installing one before the
next hunt.

## Two ProcessState races (2026-09-02, found by -race once gcc existed)

With a C compiler available, `make race` found two instances of one
idiom, both pre-existing:

    go func() { _ = cmd.Wait() }()   // reap; ProcessState marks death
    ...
    for w.cmd.ProcessState == nil { ... }   // another goroutine

`Cmd.Wait()` writes `ProcessState`, so polling that field from a
different goroutine is a data race. Sites: `dshacp` worker shutdown
against its reaper, and `live`'s Shutdown plus ensureServers against the
opencode-serve reaper.

Both now publish death by closing a channel the reaper closes — a
`managedServer{cmd, exited}` in live, an `exited chan struct{}` on the
dsh worker — and nothing reads a field Wait() owns. The dsh shutdown also
drops a 60×50ms poll for a plain 3s select.

Honest scope: these are stale-read races, not a plausible source of the
runtime fatal error from the crash. They are fixed because they are
wrong, not because they explain it.

`TestConcurrentWorkersAtScale` now drives 167 workers — the crashed run's
count — with the registry watcher spawning tailers, pollWorkers sampling,
and Shutdown tearing down mid-flight. Clean under -race across repeated
runs, which is evidence against a data race in the feed paths at that
scale. `make race` is a separate target because the detector needs cgo
and the suite must still run on a box without a C compiler.

## The context gauge assumed 200k (2026-09-02, live report)

Running fable 5.1 (1M window), the supervisor context gauge went red at
115k — 11% full. Two hardcoded constants: `supCtxWindow = 200_000` as the
denominator and `ctxWarnTokens = 100_000` as an absolute red line, both
written when 200k was the only window there was.

**The stream reports the real window.** The `result` event's `modelUsage`
map carries `contextWindow` per model:

    "modelUsage": {
      "claude-haiku-4-5-20251001": {"costUSD":0.0009, "contextWindow":200000},
      "claude-fable-5":            {"costUSD":0.164,  "contextWindow":1000000}
    }

Note both entries: the CLI runs a small helper model alongside the
session's own (already recorded above), so the window must be picked by
the model name from `system/init` — never guessed from the map, and never
by taking the largest.

So `ResultEvent.ModelWindows` now carries the map, the orchestrator
remembers the init model to resolve it, and the UI keeps
`supCtxWindow`. The denominator is that window; the warn line is half of
it, not a constant. Unknown stays 200k — conservative, and the gauge
never claims a size it wasn't told. The window is persisted in the run
ledger (`win` in sup-usage-<run>.json) next to `ctx`, for the same
reason: a resumed session must be honest before its first turn.

Flake fixed alongside: `TestShutdownKillsEverything` is the only test
that starts the full `Run` loop, so it is the only one where
`ensureServers` is live. Its endpoint is a localhost httptest URL with no
`/global/health` route, so ensureServers judged the server down and
spawned a REAL `opencode serve` against the port — which Shutdown then
had to tear down inside its budget. It failed intermittently on any box
with opencode installed. The mux now answers the health probe.

## What the supervisor and workers were actually blocked on (2026-09-02, transcript audit)

Audited every project: 8 supervisor transcripts (382 tool calls, from the
`claude` CLI's own session files — strawboss does not persist the
supervisor stream) and 175 worker transcripts (1468 tool calls).

**Workers are not blocked at all.** The composition runs
`sandbox-policy: danger-full-access` with `user-approval: never`, and the
logs agree: 879 bash calls including docker, curl, rm, kill, setsid, and
ZERO permission blocks. The only failures were 4 missing binaries
(docker, luajit, openspec) and 4 OS-level filesystem errors inside the
workload. Every "sandbox/forbidden/denied" hit on a first pass was a
false positive — those words appearing in file content the workers read.
Grepping log text for those words is worthless here; pair `tool/call`
with `tool/result` by callId instead.

**The supervisor took 23 denials in 382 calls (6%).**

| tool | denied | share of its calls |
|---|---|---|
| Grep | 10 | 19% |
| Bash, non-delegate | 7 | — |
| Bash, the delegate command itself | 3 | 3% of 93 |
| WebFetch / WebSearch | 3 | 100% |

Grep was never in the baseline — an oversight, since Read and Glob (the
same read-only class) always were. It was denied in all four projects.

**The delegate denials are not a prefix problem.** Multi-line `\`
continuations are fine: 90 of those passed. What failed was task prose
containing shell metacharacters — `$(cat …)` and `<style>` / `<script
src=…>`. Claude Code's Bash matcher splits the command on shell
operators, and the extra segments are not allowlisted, so the whole call
dies however correct the prefix is. Hence `--task-file`: a path has no
metacharacters, and the system prompt now routes hostile or long task
text through a file.

**The allowlist now also covers read-only shell** (ls, cat, head, wc,
find, git status/log/diff/show) and the web tools, per invariant 6 —
every entry was denied in a real run. Nothing that writes, builds,
installs, or executes arbitrary code was added.

**Invariant 3 is leaking, and it is expensive.** The supervisor reads
worker transcripts directly: 16 reads under `~/.strawboss/logs` pulled
~35,300 tokens into supervisor context, one of them 12,575 tokens in a
single Read. For contrast, the 93 delegate results averaged 181 tokens
each — the terse contract holding well under its ~250 target. So 16 log
reads cost about what 195 delegations cost. All 16 were in one project,
whose ledger shows a 92k footprint. Read is allowlisted, so this needs a
deny rule to actually stop, and the system prompt still says "read a log
file only when you truly need detail". Decision pending.

## Closing the invariant-3 leak: deny rules need the //abs form (2026-09-02)

The supervisor was reading worker transcripts through the allowlisted
Read tool — 16 reads, ~35,300 tokens, against delegate results averaging
181 (audit above). An allowlist cannot fix this: Read has to stay
granted, so the hole has to be carved out with `--disallowedTools`, where
deny beats allow.

**Verified against the real CLI, both ways** — the syntax is a trap:

    Read(//home/u/.strawboss/**)   BLOCKS   ← //abs form
    Read(/home/u/.strawboss/**)    ALLOWS   ← silently matches nothing

A plain absolute path is treated as relative to cwd and quietly matches
nothing; there is no error, so a wrong rule looks exactly like a working
one. Relative rules (`Read(logs/**)`) do work, but resolve against the
supervisor's cwd — the project, not the state dir. The block reports
`<tool_use_error>File is in a directory that is denied by your permission
settings.</tool_use_error>` and does NOT appear in the result's
`permission_denials` array, so that field cannot be used to confirm it.

`supervisorDisallowedTools` denies the whole state dir, which covers
worker transcripts, dsh session logs, the registry and the ledgers.
End-to-end check: a read under the state dir is blocked while a file in
the project directory still reads fine — so the supervisor's habit of
having workers fetch raw evidence into the repo and reading it itself is
untouched. Only its own scratch state is off limits.

**`cat`/`head`/`tail` were consequently left OUT of the allowlist** even
though they were denied in real runs. A Bash rule cannot be path-scoped,
so granting cat would read a transcript straight past the Read deny rule.
File content comes through Read, which honours the rules; `ls`, `wc` and
`find` stay because they leak only names and counts. The system prompt no
longer says "read a log file only when you truly need detail" — it says
the log path belongs to the human, and to delegate a follow-up task when
a result is too thin.

## The crash, identified: heap corruption in the render path (2026-09-02)

> **Superseded the same afternoon** — see "The crash, actually
> identified" below. The attribution to the toolchain or a dependency
> here was wrong; the mechanism is a `strings.Builder` inside the
> by-value Model. The evidence and the stress test remain valid.

The fd-level crash log paid off on its first crash. The cause:

    fatal error: found bad pointer in Go heap (incorrect use of unsafe or cgo?)

thrown by `runtime.badPointer` from `scanstack` → `scanframeworker` →
`scanblock`. The corrupted object's address matches the frame pointer of
`ui.Model.viewChat` exactly, so the bad pointer sits in a stack frame of
the render path: `View` → `viewChat` (chatview.go:20) → `viewChatColumn`
(chatview.go:123). The frame is ~11KB and pointer-dense because Model is
passed by value.

This is NOT a data race — `make race` is clean, including a 167-worker
stress test. It is memory corruption, and nothing in strawboss uses
`unsafe` or cgo. Among dependencies only `charmbracelet/x/ansi` uses
`unsafe`, and only to reinterpret int slices as its Param type — no
fabricated pointers, and `checkptr` (via -race) finds nothing.

Not reproduced locally: `TestRenderUnderGCStress` (internal/ui, gated
behind `STRAWBOSS_STRESS=1`) renders 4000 frames against a 160-worker
model with four allocator goroutines, under `GOGC=1
GODEBUG=gccheckmark=1` and again under -race/checkptr. Clean both ways.
Keep it as the reproducer to bisect against.

Remaining suspects, in order: the toolchain (go1.27.0 is new, and the
traceback runs through a generated allocator, `runtime/malloc_generated.go`);
then a render-path dependency. Mitigations taken rather than a fix:
`x/ansi` 0.11.6→0.11.8, `displaywidth` 0.9.0→0.11.0 (plus uax29,
go-colorful, go-runewidth), and CGO_ENABLED=0 pinned in the Makefile.
That pin matters beyond cgo: the default flips with whether a C compiler
is installed, so the build silently changed the day gcc was added. Note
the first crash predates gcc, so cgo was never the cause — pinning
removes a variable and makes the binary static.

## Startup announced failures that were already over (2026-09-02)

On launch the TUI showed "w192 failed — aborted: terminated signal
received". Those workers died with the PREVIOUS process — the crash
killed them — and the registry watcher replays that history at startup.
The remote push already guarded this (`ev.TS.After(o.started)`, "replayed
history must not re-ring anyone's phone") but the local bell and toast
did not, so a restart announced every worker the crash had killed as if
it were failing right then.

`WorkerUpsertMsg` now carries `Replay`, set from that same timestamp
comparison, and the bell is skipped for replayed failures. The row and
the logs tab still record them — only the announcement is suppressed.

Related: every toast now writes to the logs tab. A toast shows for five
seconds and vanishes, which made this one nearly unreportable; only
`ToastMsg` was logged before, not the toasts raised directly in the UI.

## Context gauge on a resumed session (2026-09-02)

The real window arrives with the first `result` event, so a session
resumed on a build that never recorded one shows the conservative 200k
until its first turn completes — then the ledger's `win` field carries it
across restarts. Verified the lookup against the live CLI: `system/init`
reports model `claude-opus-5[1m]` and `modelUsage` keys the entry
`claude-opus-5[1m]` with `contextWindow: 1000000`, an exact match, so no
name normalisation is needed for the [1m] variants.

## The crash, actually identified: a strings.Builder in a by-value Model (2026-09-02)

The third crash was a clean panic rather than a runtime throw, and it
named the mechanism:

    strings.(*Builder).copyCheck → panic("strings: illegal use of non-zero Builder copied by value")
    ui.Model.Update  model.go:375   m.streaming.WriteString(msg.Text)
    ui.Model.Update  model.go:346   the feedBatch loop, calling Update through a tea.Model interface

`Model.streaming` was a `strings.Builder`, present since M4 (07e64b3).
Bubble Tea passes Model **by value** and boxes it into a `tea.Model`
interface between updates, so every field is copied constantly. A
Builder records its own address on first write (`addr`, stored via
`abi.NoEscape` precisely so the compiler will not make it escape) and
panics when a copy writes next. That is crash 3.

It is also crashes 1 and 2. The boxed heap copy of Model carries that
`addr` field — a live pointer, as far as the GC is concerned — naming a
stack slot in a frame that no longer exists. When the GC scans a Model
copy (both traces were mid-scan of a render-path frame: `viewChat`,
called with Model by value) it finds a pointer into a dead stack region:
`found bad pointer in Go heap … pointer to unallocated span`, with the
span reported as `mSpanManual` — a stack span. One bug, two faces,
depending on whether the copy writes first (panic) or gets scanned first
(throw).

Why it stayed latent for days: `copyCheck` compares addresses, and a
receiver copy made from the same call site at the same stack depth lands
at the same address every time, so the check passed by coincidence.
Anything that shifts stack layout — a new Model field (`supCtxWindow`
was added that day), dependency bumps changing inlining, a different mix
of batched and direct Updates — breaks the coincidence. That is the
honest account of "things went downhill after the recent commits": the
bug predates them, and they changed the timing that had been hiding it.

Fix: `streaming` is a plain `string`. No other copy-hostile type lives
in Model (audited for Builder/Buffer/sync.*). `TestStreamingSurvivesModelCopies`
writes from two different stack depths through the interface, which
reproduced the live panic deterministically before the fix.

Rule for this codebase: **nothing with self-referential or
address-sensitive state goes in a Bubble Tea model.** `go vet`'s
copylocks check does not cover `strings.Builder`; the test is the guard.

The mitigations taken under the wrong theory — dependency bumps and the
CGO_ENABLED=0 pin — are kept: harmless, and the pin is right on its own
merits.

## A terminal turn message must outlive the shutdown that caused it (2026-09-02)

`TestShutdownKillsEverything` still flaked under -race after the health
route (one in four). Shutdown waits for the supervisor process to exit,
then cancels the run context; the stream goroutine emits the final
`TurnDoneEvent` through `emit(ctx, …)`, and when cancel wins that race
the "turn interrupted" message is dropped. The terminal event is now
delivered under a one-second timeout of its own rather than the run
context — it reaches any listener and never wedges a teardown with none.
Six for six under -race afterwards.

## Context gauge, round three: remember windows per model, never guess (2026-09-02)

Still red at 216k on a 1M model after the modelUsage fix. The ledger
showed why the fix could not help a live session: `"turns":15,"win":0`.
The window arrives only with a completed turn's result, and the lineforge
session's recent turns were ending in crashes and interrupts — a result
whose `modelUsage` lacks the session model yields 0, and the gauge fell
back to a guessed 200k, which reads as "bloated" at 216k. Both CLI modes
key `modelUsage` exactly as `system/init` names the model (verified
again: `claude-fable-5-1` ↔ `claude-fable-5-1`), so the lookup is right;
depending on the current run's first result was the fragile part.

Two changes:

- **Windows are remembered per model** in `<state-dir>/model-windows.json`,
  written whenever any result reports one, and applied at `system/init`
  via `SupCtxWindowMsg` — a message of its own so it cannot reset the
  live-turn counters the way `SupUsageMsg` does. A model seen once, in
  any run or project, is known from t=0 in every later session.
- **An unknown window never paints red.** The 200k default is gone; the
  dashboard shows `216.2k/?` until a window is known, and the `/new?`
  advice needs a real denominator. Red is an accusation.

When a result lists windows but not the session model, the logs tab now
says so (`context window unknown: result reports [...], session model is
"..."`) instead of silently guessing — the next report of this should be
diagnosable from the logs tab alone.
