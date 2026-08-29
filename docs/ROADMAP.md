# Roadmap — post-v1 features

Working list, priority order. One feature = one commit series, runnable and
tested at the end (same rule as milestones). Struck items are done.

1. ~~Worker kill/retry from the TUI~~ (bdf0356)
2. ~~dsh harness (DeepSeek Harness ACP workers) + tools_mode~~ (4b90e81, abaa539, 9433896)
3. ~~Worktree-per-worker~~ (`delegate --worktree`, 5e1b2de)
4. ~~`strawboss clean`~~ (retention sweep, 64248ea)
5. ~~Remote notify + control~~ — ntfy pushes and OpenClaw two-way Discord
   (inject prompts / relay replies) (db681cf, d64d067)
6. ~~Worker table filter + logs source filter~~ (31e202b; UI polish pass
   69bc0ce, 737fea0)
7. ~~Budget guard~~ (`[budget]` config, delegate stop marker, `strawboss costs`)
8. ~~Recover-all~~ (`R` retries every failed worker in the filtered view)
9. ~~Session picker / history browser~~ (`s` opens the per-project picker)
10. **Per-task auto model routing** — strawboss picks the worker model by
    task shape instead of the supervisor hand-picking (needs ≥2 models
    actually loaded on the GX10 to matter — deepseek-v4-flash still
    pending).
11. **Permission prompts in-chat** — surface supervisor permission
    requests interactively instead of pre-approving everything.

Explicitly not planned: web UI, remote/mobile control beyond OpenClaw,
MCP management — agent-deck does these; strawboss stays a local TUI with
a token-free supervisor (see the agent-deck research note).
