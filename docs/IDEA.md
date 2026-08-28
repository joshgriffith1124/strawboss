# Strawboss (working title; formerly "delegation dashboard")

**Status:** Kickoff-ready (as of 2026-08-28) — see KICKOFF.md + CLAUDE.md + MOCKUP.html in this folder
**Name:** `strawboss` — a straw boss is a worker who supervises the crew while still answering to the actual boss; exactly the supervisor's role under Josh. Runners-up: quartermaster, dugout, crewboss.
**Language:** Go (Josh's call) — Bubble Tea TUI, single static binary; supersedes the earlier Python/Textual lean.
**One-liner:** A terminal app that launches a Claude supervisor instance, watches it delegate work to opencode headless sessions on the local GX10s, and shows live worker status plus real-time token metrics — supervisor (API, costs money) vs workers (local AI, free).
**Goal level:** Zero revenue. Personal tooling / portfolio / daily driver.
**NOT the same project as agentfarm** — Josh's existing Claude→opencode delegation work lives there; this is a separate observability app that spawns its *own* supervisor. Keep them distinct.

## Context

Josh already has a Claude session delegating work to opencode headless sessions running on his local AI (2× GX10). It works, but in the Claude UI all you see is a bash delegation script running. This app makes the whole topology visible.

## Architecture — the key insight

**The TUI launches the supervisor itself, so it owns the supervisor's stdout — that IS the data feed.**

- Spawn supervisor via Claude Code headless mode (`claude -p --output-format stream-json`) or the Claude Agent SDK (Python). Every assistant message, tool call, and per-message token usage (incl. cache read/write) arrives as parsed JSON events.
- **Delegation detection is free:** the supervisor's Bash `tool_use` event for the delegation script flows through the TUI before the script runs — parse the command for prompt/target, pop a worker row into the table. The matching `tool_result` closes the row. No hooks, no instrumentation, supervisor unmodified.
- **Mid-flight worker visibility is the only gap:** while a delegation runs, the supervisor stream is silent about it. Fill it by polling the opencode side — opencode's server API on the GX10s (if sessions run against `opencode serve`) for live status/messages/tokens, or parse opencode JSON output/session storage at completion for one-shot `opencode run` calls. May require a small tweak to the delegation script to make workers queryable mid-flight.
- Worker token/cost data from opencode; supervisor from stream usage fields.

## Design principle: zero supervisor-token overhead (Josh's constraint)

Josh is agnostic on mechanism; the rule is: best functionality WITHOUT bloating supervisor tokens. Anything local in the TUI is fine.

- Observation is passive and token-free by construction: parsing supervisor stdout adds nothing to its context; polling opencode on the GX10s is free; all rendering/metrics/history local.
- Supervisor cost is determined solely by what enters its context: its own prompts + tool results. **The delegation script's return value is the lever** — it must return a TERSE result (status, few-line summary, exit code, path/session-id to full output), never the worker's full output. Full transcripts flow to the TUI locally instead. The supervisor can choose to read the full output file only when it needs to (pay-per-use detail).
- Consequence: the dashboard *enables* aggressive output-trimming on the supervisor channel — no information is lost by being terse, because everything lands in the TUI.
- Since local functionality is free: prefer `opencode serve` workers so the TUI can stream live worker transcripts + running token counts mid-flight via API polling, with zero supervisor involvement.

## v1 screen

- **Header:** the headline metric — supervisor tokens/cost (API $, cache-read vs fresh split) vs worker tokens ($0.00, local silicon); tokens/sec per GX10; active worker count.
- **Worker table:** rows appear live at delegation moment — status glyph, host, model, prompt summary, runtime, tokens.
- **Detail pane:** selected worker's output/transcript.
- **Supervisor pane:** condensed transcript to follow its reasoning.
- **Failure bell:** notification when a worker fails or supervisor errors — the secretly-killer feature (the point is NOT watching the dashboard).

## Scope decisions

- **v1 is interactive (Josh's call, supersedes earlier read-only plan):** the app IS the chat UI for the supervisor. Layout: **chat tab is the primary surface** (conversation + input box, terse delegation events inline), **side panel** carries an ambient subset (token split mini-bar, active workers, host throughput, task counts), and **a dashboard tab holds the full data** (complete worker table, live worker transcript, supervisor cost detail). Mock: https://claude.ai/code/artifact/d9fcba78-5dcd-4d00-b167-8519a9f25d6d (HTML doubles as the Textual spec).
- Interactive supervisor = one `claude -p --resume <session-id>` subprocess per user message (see auth section — Agent SDK is out on subscription auth); esc-to-interrupt via SIGINT.
- Two-host support via polling both GX10s directly (SSH/API). No collector service / message bus — overengineering at N=2.

## Model configs, not hosts (Josh's call)

The GX10 boxes themselves are irrelevant to the UI. Workers bind to **named model configs** (e.g. `~/.farm/models.toml`): each config = a name + inference endpoint + model id + **harness**. A config might be one model tensor-split across both DGXs, one per box, or a different model entirely. The TUI measures **endpoints** (tok/s, active workers, queue depth per config), never hardware. Worker table shows the config name; delegation script takes `--model <config>`. This also enables per-task model routing later (big model for gnarly refactors, small for docstrings).

## Worker harness abstraction (opencode-only v1, pluggable later)

Topology is fixed: **supervisor = claude headless → workers = local harness**. v1 workers run on **opencode only**, but qwen-code/others should be addable later without touching the TUI. Mechanism: a thin `WorkerHarness` adapter interface derived from exactly what the TUI needs and nothing more:

- `spawn(task, model_config) -> worker_id` (what the delegation script calls)
- `status(worker_id)` → queued/running/done/failed + exit summary
- `events(worker_id)` → live transcript stream (tool calls, diffs, output) for the detail pane
- `usage(worker_id)` → token counts (+ tok/s if available)
- `result(worker_id)` → terse summary + full-log path (the supervisor-channel contract)

`OpencodeHarness` (via `opencode serve` API + session storage) is the only v1 implementation; the harness name rides along in the model config and shows in the worker detail meta line. Rule: no second implementation until one is actually wanted — the interface exists to keep opencode assumptions out of the TUI code, not to speculatively support everything.

## Auth: subscription, zero marginal cost (verified 2026-08-28 vs official docs)

Josh has no API key configured — supervisor must run on his Claude subscription login. Verified:

- **`claude -p` headless works on subscription OAuth** when `ANTHROPIC_API_KEY` is unset (an env API key would take precedence — keep it unset in the TUI's env). **Do NOT use `--bare`** — bare mode doesn't use subscription login.
- **The Agent SDK is out**: docs require API-key auth and state Anthropic doesn't allow claude.ai login for products built on the SDK. So the TUI drives the `claude` CLI directly as a subprocess instead of using the SDK.
- **Interactive multi-turn without the SDK**: `--input-format stream-json` wasn't found in current headless docs (may not exist anymore) — the robust pattern is one `claude -p --resume <session-id>` invocation per user message (context persists via session resume), parsing `--output-format stream-json --include-partial-messages` each turn. Esc-to-interrupt = SIGINT (ends current turn); SIGTERM = graceful shutdown, resumable later.
- **Unattended flags**: `--permission-mode dontAsk` (or acceptEdits) + `--allowedTools` covering the delegation script, so the supervisor never blocks on a permission prompt the TUI can't show. (Revisit: TUI could also surface permission prompts in-chat later.)
- **Metering**: no programmatic plan-window/limits API on Pro/Max (analytics is Team/Enterprise). Per-invocation usage/`total_cost_usd` fields ARE in the JSON output → dashboard shows supervisor tokens + cache %, frames cost as "plan window" + notional API value, marginal $0.00. No live "x% of 5-hour window" — can't get it; consider estimating from token counts later.

## Stack

- **Go + Bubble Tea** (Josh's call, supersedes the earlier Python/Textual lean): single static binary, goroutines for the concurrent feeds (supervisor stdout, opencode polling), Elm-style message loop fits the event-driven UI. Drives the `claude` CLI as a subprocess (NOT the Agent SDK — see auth section). Details in KICKOFF.md.

## Prior art checked (Aug 2026, from PoE-era research + general knowledge — not deep-researched)

- claude-squad and similar tmux multi-agent managers: manage multiple *interactive* sessions, not cross-host delegation observability.
- opencode's own TUI: per-session only.
- Nothing found that shows a Claude supervisor + remote local-AI workers topology. Unusual setup; probably genuinely needs building.

## Next steps

1. Spike: spawn `claude -p --output-format stream-json` from Python, parse events, print tool calls + usage to console. (Proves the whole data feed.)
2. Check how the existing delegation script invokes opencode (one-shot `run` vs `serve`) — determines mid-flight visibility work.
3. Textual skeleton: header + worker table fed by the parsed stream.
