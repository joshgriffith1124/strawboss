# The delegation contract

How the supervisor spawns workers, and what flows back into its context.

## The command

```
strawboss delegate [--model <name>|auto] --task "<prompt>" [--task "<prompt>"…] [--dir <path>] [--timeout 20m]
```

- `--model` — a named entry in `~/.strawboss/models.toml` (never a hostname),
  or `auto` (the default when omitted). It is **sticky**: it binds the `--task`
  flags that follow it, so one call can mix tiers
  (`--model fast --task a --model big --task b`). See "Routing" below.
- `--task` — a worker's prompt. **Repeat it to run tasks in parallel**: each task
  becomes its own concurrent worker inside this one invocation. (This is the
  parallelism mechanism on purpose — a compound shell command like
  `delegate … & delegate … & wait` does NOT pass Claude Code's allowlist, verified
  2026-08-28.)
- `--dir` — worker working directory; defaults to the caller's cwd, so when the
  supervisor runs it from the project root, workers work in the project.
- `--timeout` — expired workers are aborted (not orphaned).
- `--state-dir` / `--models` — overrides for tests and experiments.

Blocks until every worker finishes and prints one terse result per worker. Exit 0
iff all ended `done` — otherwise exit 1, so the supervisor's tool result is
flagged as an error.

## Routing: which model a task runs on

Three decisions, all made inside the delegate call so none of them costs a
supervisor round trip:

1. **Want.** A pinned task wants the model it names. An `auto` task is scored
   from its text (`internal/router`: word count, distinct files named, heavy
   keywords like *refactor*/*debug*, light ones like *rename*/*report*) and the
   score maps onto the tiers in `models.toml` — two points per tier. Untiered
   files have nothing to map onto: auto is the first entry.
2. **Availability.** The want is probed before dispatch (dsh: the endpoint
   answers `/models` and lists the model; opencode: the server answers). A
   model that is down or not loaded is routed around: same-tier sibling first,
   then the nearest tier up, then down as a last resort. Each model is probed
   once per call; if nothing is available the task runs on its want and
   surfaces the real error.
3. **Escalation.** A failed task retries once on the next tier up (next file
   entry when untiered), skipping models that probe unavailable at that moment.

The supervisor is told about a decision only when it could not have known it —
one bracketed line ahead of the summary:

```
[auto → fast-local: score 0 → tier 1]
[fast-local unavailable (endpoint unreachable); ran on qwen-dsh]
[escalated from fast-local, which failed: …]
```

## Verify: checking a worker's claim

Workers report their own success, and the supervisor may not run builds or
tests. So a project can name a check in `.strawboss.toml` at its root (looked
up from `--dir` upward, stopping at the git root):

```toml
verify = "go test ./..."
verify_timeout = "10m"   # optional, default 5m
```

`--verify "<cmd>"` overrides it for one call; `--verify off` skips it.

- **Per worker** when each worker has a directory to itself (`--worktree`, or a
  single task): the check runs after the worker reports done and before the
  result is recorded. A failure turns the worker `failed` (registry, TUI and
  supervisor agree), escalates like any failure, and the escalated attempt's
  prompt carries the failing lines.
- **Once, after all workers** when several share one directory: each would
  otherwise be judged on the others' half-finished edits. It can't say which
  worker broke the build, so statuses stand and the call exits non-zero.

The supervisor reads one line — `[verified: `go test ./...` passed in 12s]` —
or `[verify FAILED: … exit 1 after 8s]` plus the last few output lines
(≤ 400 bytes).

## What the supervisor sees (the terse-result contract)

stdout is the ONLY thing that enters supervisor context. Two parts, ≤ ~250 tokens:

```
w3 done 5s · log /home/josh/.strawboss/logs/ses_….jsonl
Created hello.txt containing `delegated hello` and confirmed via cat.
```

Line 1: worker id, status (`done`/`failed`), wall time, full-log path.
Rest: the worker's few-line summary (capped at 700 bytes).

The supervisor can `Read` the log path when it genuinely needs detail —
pay-per-use. Everything else (live transcript, token counts, registry events)
reaches the TUI locally, costing the supervisor nothing.

## Supervisor flags that make it work unattended

The supervisor must never block on a permission prompt (invariant 6). Spawn it with:

```
--permission-mode acceptEdits
--allowedTools "Bash(strawboss delegate:*),Read"
```

- `Bash(strawboss delegate:*)` pre-approves every delegate invocation and nothing
  else Bash-shaped.
- `Read` lets it inspect worker logs and project files.
- Widen deliberately (e.g. `Bash(go test:*)` if the supervisor should verify
  results itself); every widening is more the supervisor can do unprompted.
- `strawboss` must be on the supervisor's PATH (or use an absolute path in the
  system prompt telling it how to delegate).

## What lands on disk (all under `~/.strawboss/`)

- `workers.jsonl` — append-only registry: one `spawned` and one `finished` event
  per worker (id, session, model, task, dir, status, summary, log path, duration,
  tokens). Events carry a `run` id ($STRAWBOSS_RUN, set by the TUI on the
  supervisor and inherited by delegate): the TUI replays only its own run's
  events, so resuming shows that run's workers while `--new` starts clean.
  Concurrent delegations are safe (flock).
- `run` — the current run id (rotated by `--new`).
- `logs/<session>.jsonl` — the full worker transcript, one message per line.
