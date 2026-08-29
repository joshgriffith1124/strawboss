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

### DeepSeek Harness workers

dsh is young (developer preview) and its setup is currently manual: the
acp profile needs the `dsh-acp-demo` app packages installed (`dsh plugin
--profile acp add @deepseek-ai/dsh-acp-demo @deepseek-ai/dsh-acp
@deepseek-ai/dsh-agent-spine-demo` plus leaf plugins). `docs/NOTES.md`
has the verified, step-by-step findings — including the wire quirks
strawboss works around for you (an sglang/dsh tool-call streaming
incompatibility, reasoning-effort vocabulary clashes).

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
