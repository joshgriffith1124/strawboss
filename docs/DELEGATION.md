# The delegation contract

How the supervisor spawns workers, and what flows back into its context.

## The command

```
strawboss delegate --model <name> --task "<prompt>" [--dir <path>] [--timeout 20m]
```

- `--model` — a named entry in `~/.strawboss/models.toml` (never a hostname).
- `--task` — the worker's prompt.
- `--dir` — worker working directory; defaults to the caller's cwd, so when the
  supervisor runs it from the project root, workers work in the project.
- `--timeout` — the worker is aborted (not orphaned) when it expires.
- `--state-dir` / `--models` — overrides for tests and experiments.

Blocks until the worker finishes. Exit 0 iff the worker ended `done` — a failed or
aborted worker exits 1, so the supervisor's tool result is flagged as an error.

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
  tokens). The TUI replays this; state survives restarts. Concurrent delegations
  are safe (flock).
- `logs/<session>.jsonl` — the full worker transcript, one message per line.
