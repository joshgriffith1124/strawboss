# Strawboss — Project Kickoff

> A straw boss is a worker who supervises the crew while still answering to the actual boss.
> `strawboss` is a terminal app that runs a Claude supervisor which delegates coding work to
> local AI workers (opencode on Josh's GX10 boxes) — and makes the whole operation visible.

**This file + `CLAUDE.md` + `IDEA.md` + `MOCKUP.html` are the kickoff package.** Copy them into a
fresh git repo (`CLAUDE.md` at repo root; the others under `docs/`) and start a session with the
first prompt at the bottom of this file.

- `IDEA.md` — full background: problem, architecture rationale, verified constraints, research.
- `MOCKUP.html` — the UI spec (open in a browser): two screens, chat tab + dashboard tab.
- `CLAUDE.md` — repo instructions for Claude Code sessions (invariants live there).

## What v1 is

A Go TUI (`strawboss`) that:

1. **Spawns and chats with a Claude Code supervisor** (headless CLI, subscription auth) — chat is
   the primary tab: your messages, its replies, delegation events inline as terse one-liners, an
   input box, esc-to-interrupt.
2. **Shows delegations live** — workers appear the moment the supervisor calls the delegate
   command; side panel carries the ambient subset (token split, active workers, model endpoints);
   a dashboard tab holds full data (worker table, live worker transcript, supervisor detail);
   a logs tab holds raw streams. Bell on worker failure.
3. **Tracks the token economy** — supervisor tokens (plan-metered, marginal $0.00, notional API
   value) vs worker tokens (local, unmetered), per-endpoint tok/s.

Topology: **supervisor = `claude -p` headless → workers = opencode** (harness-pluggable later).

## Stack

- **Go**, single module, single binary.
- TUI: **Bubble Tea** + Lipgloss + Bubbles (charmbracelet). Elm-style message loop; every external
  feed (supervisor stdout, opencode polling) is a goroutine sending typed `tea.Msg`s.
- Config: TOML (`BurntSushi/toml`) — `~/.strawboss/config.toml` + `models.toml`.
- No web server, no database. State in memory; logs as JSONL files under `~/.strawboss/logs/`.
- Tests: `go test`; format/lint: `gofmt` + `go vet` (add golangci-lint only if it earns its keep).

## Verified constraints (do not rediscover these — checked against official docs 2026-08-28)

- `claude -p` headless **works on subscription OAuth** iff `ANTHROPIC_API_KEY` is UNSET in the
  subprocess env and `--bare` is NOT used. Strawboss must scrub the env var when spawning.
- **Agent SDK is not usable** (requires API key; claude.ai login disallowed for SDK-built tools).
  Drive the CLI subprocess directly.
- Multi-turn pattern: **one `claude -p --resume <session-id>` invocation per user message**, with
  `--output-format stream-json --include-partial-messages`. First turn creates the session; parse
  the session id from the stream. SIGINT ends a turn (esc-to-interrupt); SIGTERM = graceful,
  resumable shutdown.
- Unattended operation: `--permission-mode` + `--allowedTools` must cover everything the
  supervisor needs (the delegate command, file reads) — it can never block on a prompt the TUI
  doesn't render. v1: pre-approve; surfacing permission prompts in-chat is a later feature.
- No programmatic plan-window/limit data on Pro/Max. Track tokens from stream usage fields;
  show notional API value; never pretend to know "% of 5-hour window."

## Architecture (match `IDEA.md`; summary)

```
┌────────────────────────── strawboss (Go, one process) ──────────────────────────┐
│  tea.Program (UI model: tabs, chat, worker table, metrics)                      │
│    ▲ tea.Msg                ▲ tea.Msg                    ▲ tea.Msg              │
│  supervisor driver        harness poller               config/watcher           │
│  (spawn claude -p,        (opencode serve API +        (models.toml,            │
│   parse stream-json,       session storage →            allowedTools,           │
│   detect delegate          worker status/events/        keybinds)               │
│   tool_use events)         usage)                                               │
└─────────┬───────────────────────┬───────────────────────────────────────────────┘
          │ subprocess            │ HTTP (LAN)
     claude CLI              opencode serve on GX10s  ←── delegate command spawns
     (subscription auth)     (model configs: endpoint + model + harness)          
```

Key contracts:

- **Terse-result contract:** the delegate command returns to the supervisor ONLY
  `{worker_id, status, few-line summary, full-log path}`. Full transcripts go to the TUI via the
  harness, never into supervisor context. Target: avg ≤ ~250 tokens per delegation result.
- **`WorkerHarness` interface** (v1: opencode only; boundary keeps harness specifics out of UI):
  `Spawn(task, modelConfig) → workerID` · `Status(id)` · `Events(id) → stream` · `Usage(id)` ·
  `Result(id) → terse summary + log path`.
- **Model configs, not hosts:** workers bind to named entries in `models.toml`
  (`name, endpoint, model, harness`); the TUI measures endpoints (tok/s, active, queue), never
  hardware.

## Milestones (each ends runnable + tested; commit per milestone)

- **M0 — repo skeleton.** Go module, `cmd/strawboss`, `internal/{supervisor,harness,ui,config}`,
  CI-less Makefile (`build test fmt`), config loading with a `models.toml` example.
- **M1 — supervisor driver (the de-risking spike).** `internal/supervisor`: spawn
  `claude -p --output-format stream-json`, env-scrubbed; parse events into typed structs
  (assistant text, tool_use, tool_result, usage); session-id capture + `--resume` continuation;
  SIGINT interrupt. Prove with a console harness: two-turn conversation printed as parsed events
  with running token totals. **If this milestone fails, the project pivots — do it first.**
- **M2 — opencode harness.** `internal/harness/opencode`: implement the interface against
  `opencode serve` (+ session storage fallback). Prove: spawn a trivial task on a GX10, stream
  its events, read usage and result.
- **M3 — delegation contract.** The `delegate` entrypoint the supervisor calls (subcommand:
  `strawboss delegate --model qwen-coder --task "..."`), terse-result output, `--allowedTools`
  documentation, worker registry (in-memory + JSONL event log so state survives restarts).
- **M4 — TUI shell.** Bubble Tea app with tabs/chat/side-panel/dashboard per `MOCKUP.html`,
  driven by a fake event source (recorded M1/M2 streams replayed) so UI work doesn't burn GX10
  time. Match the mock's layout, colors, and status glyphs.
- **M5 — integration.** Real feeds wired, failure bell, logs tab, esc-interrupt, graceful
  shutdown/resume. **v1 done when:** Josh runs `strawboss`, gives the supervisor a task, watches
  ≥2 workers execute on the GX10s with live transcripts and correct token split, gets a bell on
  a failure, and total supervisor context stays terse (spot-check avg tokens/delegation result).

## Later (explicitly out of v1)

Multi-harness (qwen-code etc.), per-task auto model routing, permission prompts in-chat,
plan-window estimation, session picker/history browser, worker kill/retry from the TUI.

## Suggested first prompt for the new session

> Read CLAUDE.md, docs/KICKOFF.md, and docs/IDEA.md. Set up M0 (repo skeleton) and then start
> M1 (supervisor driver spike) per the milestone plan. Ask nothing you can decide from the docs.
