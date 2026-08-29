# Roadmap — post-v1 features

Working list, priority order. One feature = one commit series, runnable and
tested at the end (same rule as milestones). Struck items are done.

1. ~~Worker kill/retry from the TUI~~ (bdf0356)
2. ~~dsh harness (DeepSeek Harness ACP workers) + tools_mode~~ (4b90e81, abaa539, 9433896)
3. **Worktree-per-worker** — `delegate --worktree`: each worker runs in an
   isolated git worktree on its own `strawboss/*` branch so parallel
   workers can't clobber the shared checkout; work is committed on the
   branch and reported in the terse result; merging stays a human
   decision (strawboss never commits to the user's branch). Inspired by
   agent-deck (scratchpad research, 2026-08-29); ours is delegate-level,
   not session-level.
4. **`strawboss clean`** — retention sweep for `~/.strawboss/logs`,
   `dsh-sessions`, and stale worktrees (age-based, manual command, never
   automatic).
5. **ntfy push on worker failure** — optional `[notify]` config; keeps the
   bell, adds a phone ping; zero supervisor tokens.
6. **Worker table filters + search** — `/` fuzzy filter, status filters
   (running/failed) on the dashboard.
7. **Per-task auto model routing** — strawboss picks the worker model by
   task shape instead of the supervisor hand-picking (wants ≥2 models
   actually loaded to matter).
8. **Session picker / history browser** — browse and resume past
   supervisor sessions and replay recorded runs from the TUI.
9. **Permission prompts in-chat** — surface supervisor permission
   requests interactively instead of pre-approving everything.
10. **Budget guard** — per-run notional-cost / plan-window ceiling with a
    hard stop; `costs recompute` from the JSONL history.
11. **Recover-all** — one-key restart of every down worker (double-probe
    liveness before declaring death).

Explicitly not planned: web UI, remote/mobile control, MCP management —
agent-deck does these; strawboss stays a local TUI with a token-free
supervisor (see the strategic note in the agent-deck research).
