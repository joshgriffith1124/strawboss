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
    actually loaded on the GX10 to matter — it serves one at a time;
    glm-5.3-flash since 2026-09-06). Landed 2026-09-21 as tiered models:
    `tier` + `description` in models.toml, `--model auto` scoring each
    task onto a tier, sticky per-task `--model` (NOTES.md "Tiered
    models"). Open: tune the score weights against real failure data.
11. ~~Permission prompts (lite)~~ — denials are loud (chat note with a
    paste-ready allowed_tools fix, toast, remote push). Full interactive
    prompts deferred until the denial log shows they'd pay.
12. ~~Escalation skips dead models~~ — delegate probes availability
    before dispatch and before escalating, and routes around a model
    that is unreachable or not loaded (NOTES.md "Tiered models").
13. **EDNS-proof name resolution** — retry `LookupIPAddr` with a plain
    (no-EDNS) query when it fails, so LAN hostnames survive routers that
    answer EDNS A queries with a bogus record (proven with raw queries,
    NOTES.md "EDNS"; low priority now that the inference host's IP is a
    DHCP reservation); then
    revert models.toml endpoints from IPs back to hostnames.
14. ~~Hot-reload models.toml~~ — the TUI reads it once at startup while
    delegate re-reads it per call; the gap crashed the TUI on 2026-09-21
    (NOTES.md "The crash it caused"). Watch the file and swap the model
    set safely so the Models pane and the supervisor's view follow edits.
15. ~~Model scorecard~~ — `strawboss stats`: success rate, escalation
    rate and median time per model and per auto-score band, from
    workers.jsonl; the same figures in the Models pane. The data item
    10's weight tuning needs.
16. **Per-model concurrency cap** — optional `max_concurrent` in
    models.toml; tasks over the cap wait as "queued" in the worker table
    instead of thrashing one GPU (IDEA.md failure point 4).
17. **Verify gate** — a per-project `verify` command (or `--verify`) run
    after each worker; a failure marks the worker failed, escalates, and
    returns a terse excerpt. Workers claim success; the supervisor cannot
    run tests to check.

Explicitly not planned: a local-TUI/remote-execution split (`--remote
host:/path`). Everything that touches code is one co-located unit —
supervisor, delegate, workers, state — so remote code means remote
agents, and once the remote box is a full strawboss host anyway, `ssh
-t` + tmux gets the same result for free (README: "Working on a remote
machine"). The only shape ssh can't cover is one TUI watching several
boxes at once; revisit if that need is real. Also not planned: web UI,
remote/mobile control beyond OpenClaw, MCP management — agent-deck does
these; strawboss stays a local TUI with a token-free supervisor (see the
agent-deck research note).
