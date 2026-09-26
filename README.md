# strawboss

> A straw boss is a worker who supervises the crew while still answering to
> the actual boss.

**A Claude Code supervisor that delegates coding work to free local AI
workers — with a terminal UI that shows the whole operation.**

You chat with one Claude supervisor (running on your existing Claude
subscription — marginal cost **$0.00**, no API key). It breaks your request
into tasks and delegates them to workers running on your own hardware
(local models via [opencode](https://opencode.ai) or
[DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) against
any OpenAI-compatible endpoint). The TUI shows the live worker table,
streaming transcripts, and the token economy: what the plan-metered
supervisor spent vs. what your free local workers churned through.

Anything you type that starts with `/` goes to the supervisor as a Claude
Code slash command — `/loop 10m Run one iteration of LOOP.md`, `/compact`,
`/context`, your own `.claude/commands` skills. Tab completes from the
list the CLI advertises; `/new` (fresh session) is the one strawboss
handles itself. A `/loop` lives only as long as the supervisor process:
`/new` or a crash drops it (the chat says so when one is scheduled). Esc
interrupts the current turn in place: the supervisor process, its running
workers, and any loop stay up.

![The chat tab: supervisor conversation, inline delegations, and the token
economy — 180k fresh plan tokens steering 4M free local
tokens](docs/screenshot-chat.png)

![The dashboard: live worker table, per-worker transcript with tok/s and
context usage, supervisor detail with recent delegation
results](docs/screenshot-dashboard.png)

```
┌────────────────────────── strawboss (Go, one process) ────────────────────┐
│  Agents (chat + worker panes) · Tasks · Worktrees · Models · Analytics    │
│    ▲                        ▲                          ▲                  │
│  supervisor driver        worker harnesses           registry watcher     │
│  (headless `claude -p`,   (opencode serve API ·      (workers.jsonl,      │
│   stream-json parsed,      dsh ACP over stdio)        replayable)         │
│   $0 marginal cost)                                                       │
└─────────┬─────────────────────┬───────────────────────────────────────────┘
          │ subprocess          │ HTTP / stdio
     claude CLI            local workers → your OpenAI-compatible endpoint
     (subscription auth)   (qwen, deepseek, … on your own GPU box)
```

## Why it's built this way

- **Subscription auth only.** The supervisor is the plain `claude` CLI with
  `ANTHROPIC_API_KEY` scrubbed — it meters against your Claude plan, never
  an API bill.
- **Zero-token observability.** The TUI watches passively: it parses the
  supervisor's stdout and tails worker logs. Heartbeats, polling, and
  notifications never inject a token into the supervisor's context (a
  deliberate contrast to orchestrators that steer their conductor by
  messaging it).
- **Terse-result contract.** A delegation returns ~250 tokens to the
  supervisor: worker id, status, a few-line summary, a log path. Full
  transcripts flow to the TUI out-of-band, so the supervisor's context
  stays small and cheap.

## Features

- Six tabs (`1`–`6` or `tab`): **Agents** — the supervisor chat beside
  live worker panes and a right-hand sidebar of agents and the token
  economy (`shift+tab` cycles panes, `ctrl+o` zooms one, `ctrl+t` hides
  the worker tiles); **Tasks**, **Worktrees**, **Models**, **Analytics**,
  **Logs**
- Live worker panes with streaming transcripts, per-worker tok/s
  sparklines, context-window usage, kill (`x`) / retry (`r`) / retry-all
  (`R`), undo a worker's edits (`u`, press twice), filtering (`/`); a
  context gauge on the supervisor pane says when a fresh session (`/new`)
  would be cheaper
- Paste images into the supervisor chat (`alt+v`, or `ctrl+v` where the
  terminal passes it through) — screenshots go in as image blocks, like
  the Claude Code TUI. Reads the clipboard via PowerShell on WSL,
  `osascript` on macOS, `wl-paste` or `xclip` on Linux
- Token economy: plan vs. free-local split, notional API value, real
  plan-window gauges (5h/7d) parsed from the stream
- Two worker harnesses: opencode server sessions and DeepSeek Harness
  (ACP), with per-model `tools_mode` (native / code / both)
- `--worktree` isolation: each parallel worker in its own git worktree +
  branch; nothing clobbers your checkout, nothing merges without you
- Per-project sessions with a picker (`s`) — resume any past conversation
  and its worker history
- Budget guard: notional-cost / plan-window ceilings that warn, then block
  new delegations with a terse refusal the supervisor understands
- Remote reach (entirely optional): worker failures push via
  [ntfy](https://ntfy.sh) or an [OpenClaw](https://openclaw.ai) channel
  (e.g. Discord) — and with two-way enabled, replying in the channel
  steers the supervisor from your phone. Unconfigured, you keep the
  terminal bell and nothing external is ever contacted
- Honest results: a worker that leaves stray files behind (debug tests,
  scratch scripts) has them listed in its result; a worker blocked on an
  opencode permission prompt fails in seconds with the reason instead of
  hanging to its deadline; a worker whose process died in a crash is
  marked "process lost" on the next start instead of "running" forever
- Loud permission denials with paste-ready allowlist fixes; `strawboss
  clean` retention sweeps; `strawboss costs` per-run summaries;
  `strawboss models` (which models are up, their tiers and busy slots)
- `models.toml` is watched: edit it while strawboss runs and the Models
  tab and the supervisor's routing follow
- Inside a herdr (terminal workspace manager) pane, strawboss reports itself to
  herdr's agent sidebar (working/idle, worker and token counts, a pop-up
  when a worker fails); outside herdr this does nothing
- Everything replayable: state is JSONL under `~/.strawboss/`

## Setup

Requirements: Go 1.27+, a [Claude Code](https://claude.com/claude-code)
subscription login, an OpenAI-compatible inference endpoint (sglang, vLLM,
llama.cpp server, …), and at least one worker harness:
[opencode](https://opencode.ai) and/or DeepSeek Harness (`npm i -g
@deepseek-ai/dsh` — see the dsh notes below).

```sh
git clone https://github.com/joshgriffith1124/strawboss
cd strawboss && make build

mkdir -p ~/.strawboss
cp examples/models.toml ~/.strawboss/models.toml   # edit: your endpoint + models
cp examples/config.toml ~/.strawboss/config.toml   # optional: notify/budget

./bin/strawboss
```

Type what you want built. The supervisor delegates; its workers appear as
live panes beside the chat on the Agents tab. `models.toml` order is preference order — the
supervisor favors the first entry.

**Tiered models.** With more than one model — say a small fast one next to
a big slow one — give each entry a `tier` (1 = cheapest/fastest, higher =
stronger) and a one-line `description` of what it is good for. The
supervisor then routes each task to the lowest tier whose description
fits — or passes `--model auto` and strawboss scores the task onto a tier
itself — and a failed task escalates once to the next tier up. A model
that is unreachable or not loaded is routed around automatically, so a
fast model on a workstation that comes and goes is safe to list.
Optional per-entry `max_concurrent` caps how many workers a model runs at
once (extra tasks wait as "queued", or go to a free same-tier sibling),
and `timeout` sets that model's per-worker deadline (default 20m).
`examples/models.toml` has a worked pair; `docs/DELEGATION.md` has the
routing rules.

**Verify.** Put `verify = "go test ./..."` (or your build/lint/test
command) in a `.strawboss.toml` at the project root and delegate runs it
after workers finish: a worker whose check fails is reported failed and
escalated with the failing lines, so the supervisor stops taking a small
model's "done" on faith. Parallel workers sharing one directory are
checked once after all of them, and a failure names the workers whose
files the failing output points at. `strawboss check` runs the check by
hand; `strawboss stats` shows how each model is doing.

**Branch review.** Work from `delegate --worktree` lands on `strawboss/*`
branches. Press `b` in the TUI to list them with diff stats, `enter` to
read a diff, `m` to merge one into your branch, `x` to discard it.

### Working on a remote machine

Run strawboss *on* the box that holds the code — not locally against it.
Everything that touches files is one co-located unit: the supervisor
(`claude -p`) runs with the repo as its cwd, calls `strawboss delegate`
through its own Bash tool, and delegate spawns workers in that same
filesystem. There is no useful split where the agents run here and the
code lives there.

So set the remote box up as a normal strawboss host — binary, `claude`
logged in on subscription auth, a worker harness, `~/.strawboss/models.toml`
— and attach to it:

```sh
ssh -t remote-box 'cd /path/to/repo && tmux new -A -s strawboss strawboss'
```

tmux (or mosh) matters: a dropped ssh session would otherwise take the TUI
and the run with it. Detach with `C-b d`, reattach with the same command.

Cross-compiling the binary for that box is enough — it needs no Go
toolchain of its own. On a network doing TLS inspection, Node is the part
that breaks (npm, opencode, dsh, and the `claude` CLI all ship their own
CA bundle); `docs/NOTES.md` has the verified fix, along with the macOS
and prerelease-pinning specifics.

The inference endpoint does not have to live on that box — `models.toml`
endpoints may point anywhere the box can reach, and strawboss only manages
`opencode serve` for localhost endpoints.

### Worker harness setup (at least one required)

Workers don't run themselves: each `models.toml` entry names a harness,
and that harness must be installed and able to reach your inference
endpoint. Without one, every delegation fails at spawn.

**Option A — opencode** (the simpler path):

1. Install [opencode](https://opencode.ai) so `opencode` is on your PATH.
2. Point it at your inference endpoint in
   `~/.config/opencode/opencode.json`:

   ```json
   {
     "provider": {
       "local": {
         "npm": "@ai-sdk/openai-compatible",
         "options": { "baseURL": "http://your-inference-host:8000/v1", "apiKey": "local" },
         "models": { "qwen3.8-27b": { "limit": { "context": 262144, "output": 49152 } } }
       }
     }
   }
   ```

3. Your `models.toml` entry then uses `endpoint = "http://127.0.0.1:4477"`
   (the opencode server) and `model = "local/qwen3.8-27b"`. You do NOT
   need to run `opencode serve` yourself — strawboss spawns and babysits
   it for any localhost endpoint that fails its health check.
4. Let workers reach files outside the project. opencode asks before a
   tool touches a path outside its working directory, and an unattended
   worker cannot answer — add this to the same `opencode.json`:

   ```json
   "permission": { "external_directory": "allow" }
   ```

   Without it, such a worker is aborted with a "blocked on an opencode
   permission prompt" result.

**Option B — DeepSeek Harness (dsh)**: powerful but young (developer
preview), and its setup is currently manual:

1. `npm i -g @deepseek-ai/dsh@0.1.1-rc.2` — the version strawboss is
   verified against; 0.1.5-rc.3 breaks the worker harness (NOTES.md)
2. Install the ACP app packages into the acp profile, pinned to the SAME
   version (append `@0.1.1-rc.2` to each — an unpinned spec resolves the
   wrong dist-tag, and a global/profile version mismatch stops every dsh
   worker at startup):
   `dsh plugin --profile acp add @deepseek-ai/dsh-acp-demo
   @deepseek-ai/dsh-acp @deepseek-ai/dsh-agent-spine-demo
   @deepseek-ai/dsh-llm-deepseek @deepseek-ai/dsh-fs-sandbox
   @deepseek-ai/dsh-sandbox-policy @deepseek-ai/dsh-system-prompt
   @deepseek-ai/dsh-user-approval`
3. Your `models.toml` entry points straight at the LLM:
   `endpoint = "http://your-inference-host:8000/v1"`, `model` = the id
   your server serves, `harness = "dsh"`. strawboss generates the worker
   composition (`strawboss.cordis.yml`) into the profile on first use and
   works around the wire quirks for you (an sglang/dsh tool-call
   streaming incompatibility, reasoning-effort vocabulary clashes).

`docs/NOTES.md` has the verified, step-by-step findings behind all of
this. `examples/models.toml` shows both harness flavors side by side.

## Claude usage & terms

strawboss drives the official `claude` CLI through its documented
headless interface (`-p`, stream-json, `--resume`) on **your own
subscription login** — no API-key requirement, no scraping, no metering
tricks; usage draws from your plan and the TUI shows the plan-window
utilization Anthropic reports. It deliberately does **not** use the
Agent SDK, which requires API-key auth (OAuth from consumer accounts is
disallowed there) — that boundary is why invariant #1 exists.

Know the landscape before relying on it: through 2026 Anthropic has been
reworking how programmatic (`claude -p` / third-party tool) usage meters
against subscriptions — a capped monthly credit pool was announced, then
paused, and may return in some form. That's a billing question, not a
permission one, but "marginal $0.00" may some day mean "within a monthly
programmatic allowance" (the built-in budget guard helps). What clearly
crosses the line, regardless: running strawboss as a service for others
on your subscription, always-on unattended daemon farms, or anything
that spoofs the CLI. Keep it what it is — one human, steering their own
account, interactively. Read Anthropic's current consumer terms
yourself; they change.

## Honest maturity notes

This is young software, built fast and verified against real binaries as
it went (every finding is dated in [`docs/NOTES.md`](docs/NOTES.md) — the
lab journal is half the value of the repo). It has one daily user so far.
The invariants above are load-bearing and tested; the edges (dsh setup,
OpenClaw integration) assume you enjoy plumbing. Issues and PRs welcome —
`make build test fmt` must pass.

- [`docs/IDEA.md`](docs/IDEA.md) — full design rationale
- [`docs/KICKOFF.md`](docs/KICKOFF.md) — original milestone plan
- [`docs/ROADMAP.md`](docs/ROADMAP.md) — what's done, what's next
- [`docs/NOTES.md`](docs/NOTES.md) — verified-against-reality findings

MIT licensed.
