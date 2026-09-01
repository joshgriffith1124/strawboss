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

![The chat tab: supervisor conversation, inline delegations, and the token
economy — 180k fresh plan tokens steering 4M free local
tokens](docs/screenshot-chat.png)

![The dashboard: live worker table, per-worker transcript with tok/s and
context usage, supervisor detail with recent delegation
results](docs/screenshot-dashboard.png)

```
┌────────────────────────── strawboss (Go, one process) ────────────────────┐
│  chat tab · dashboard (worker table + live transcripts) · logs            │
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

- Live worker table with streaming transcripts, per-worker tok/s
  sparklines, context-window usage, kill (`x`) / retry (`r`) / retry-all
  (`R`), filtering (`/`)
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
- Loud permission denials with paste-ready allowlist fixes; `strawboss
  clean` retention sweeps; `strawboss costs` per-run summaries
- Everything replayable: state is JSONL under `~/.strawboss/`

## Setup

Requirements: Go 1.22+, a [Claude Code](https://claude.com/claude-code)
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

Type what you want built. The supervisor delegates; the dashboard (tab 2)
shows workers live. `models.toml` order is preference order — the
supervisor favors the first entry.

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

**Option B — DeepSeek Harness (dsh)**: powerful but young (developer
preview), and its setup is currently manual:

1. `npm i -g @deepseek-ai/dsh`
2. Install the ACP app packages into the acp profile:
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
