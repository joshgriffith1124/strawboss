# Strawboss

Go TUI that runs a Claude Code supervisor (headless CLI, subscription auth) which delegates
coding tasks to local AI workers (opencode on the GX10 boxes) and visualizes the whole operation:
chat with the supervisor, live worker table, supervisor-vs-local token economy.

Docs: `docs/KICKOFF.md` (milestones, stack), `docs/IDEA.md` (full design rationale),
`docs/MOCKUP.html` (UI spec — match its layout, colors, and glyphs).

## Invariants — do not violate without Josh's sign-off

1. **Subscription auth only.** Spawn the `claude` CLI with `ANTHROPIC_API_KEY` scrubbed from the
   subprocess env. Never pass `--bare`. Never use the Agent SDK (requires API key). Marginal
   supervisor cost must stay $0.00.
2. **Zero supervisor-token overhead from observability.** The TUI observes passively (parse
   supervisor stdout; poll opencode locally). Nothing the TUI does may inject tokens into the
   supervisor's context.
3. **Terse-result contract.** The delegate command returns to the supervisor only:
   worker id, status, a few-line summary, and the full-log path. Full worker transcripts flow to
   the TUI through the harness — never through the supervisor. Keep avg result ≤ ~250 tokens.
4. **Harness boundary.** UI code never talks to opencode directly — only through the
   `WorkerHarness` interface (Spawn/Status/Events/Usage/Result). No second harness
   implementation until one is actually wanted.
5. **Model configs, not hosts.** Workers reference named entries in `models.toml`
   (name/endpoint/model/harness). No hostnames in UI code or display logic.
6. **The supervisor must never block on an interactive permission prompt** — keep
   `--permission-mode`/`--allowedTools` covering everything it's expected to do.

## Stack & conventions

- Go, single module, single binary. TUI: Bubble Tea + Lipgloss + Bubbles. Config: TOML.
- Layout: `cmd/strawboss/` (main + subcommands incl. `delegate`), `internal/supervisor/`,
  `internal/harness/` (`opencode/` under it), `internal/ui/`, `internal/config/`.
- External feeds are goroutines emitting typed `tea.Msg`s into the program; no shared mutable
  state with the UI model outside the message loop.
- Logs/state: JSONL under `~/.strawboss/` — human-greppable, replayable (M4 drives the UI from
  recorded streams).
- `make build test fmt` must pass before any commit. Table-driven tests; parsers get fixture
  files from real captured streams.
- Errors: wrap with context (`fmt.Errorf("...: %w", err)`); a dead worker or dropped stream is a
  displayed state, never a crash.

## Working style

- Follow the milestone order in `docs/KICKOFF.md` (M1 supervisor driver before any UI work —
  it's the de-risking spike). One milestone = one commit series, runnable and tested at the end.
- When a Claude CLI flag or opencode API detail doesn't behave as documented in KICKOFF.md,
  verify against the real binary/server and record the finding in `docs/NOTES.md` rather than
  guessing.
- UI changes: compare against `docs/MOCKUP.html` before calling done.
