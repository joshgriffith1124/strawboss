package live

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"strawboss/internal/config"
	"strawboss/internal/supervisor"
	"strawboss/internal/ui"
)

// Orchestrator runs the live feeds and exposes them as one tea.Msg channel.
type Orchestrator struct {
	Driver   *supervisor.Driver
	Models   []config.ModelConfig
	StateDir string

	feed      chan tea.Msg
	prompts   chan string
	interrupt chan struct{}

	mu              sync.Mutex
	sessionToWorker map[string]string // opencode session id → wN
	workerSession   map[string]string // wN → opencode session id
	workerModel     map[string]string // wN → model config name
	workerDir       map[string]string // wN → working directory (for scoped status)
	unfinished      map[string]bool   // wN spawned but no finished event yet
}

// New builds an orchestrator; call Run to start the feeds.
func New(d *supervisor.Driver, models []config.ModelConfig, stateDir string) *Orchestrator {
	return &Orchestrator{
		Driver:          d,
		Models:          models,
		StateDir:        stateDir,
		feed:            make(chan tea.Msg, 64),
		prompts:         make(chan string, 4),
		interrupt:       make(chan struct{}, 1),
		sessionToWorker: map[string]string{},
		workerSession:   map[string]string{},
		workerModel:     map[string]string{},
		workerDir:       map[string]string{},
		unfinished:      map[string]bool{},
	}
}

// Feed is the channel the UI listens on.
func (o *Orchestrator) Feed() <-chan tea.Msg { return o.feed }

// OnPrompt queues a user prompt for the supervisor (ui hook).
func (o *Orchestrator) OnPrompt(text string) {
	select {
	case o.prompts <- text:
	default:
		o.emitAsync(ui.RawLogMsg{Source: "app", Line: "prompt queue full — dropped"})
	}
}

// OnInterrupt requests an interrupt of the in-flight turn (ui hook).
func (o *Orchestrator) OnInterrupt() {
	select {
	case o.interrupt <- struct{}{}:
	default:
	}
}

func (o *Orchestrator) emit(ctx context.Context, msgs ...tea.Msg) {
	for _, m := range msgs {
		select {
		case o.feed <- m:
		case <-ctx.Done():
			return
		}
	}
}

func (o *Orchestrator) emitAsync(m tea.Msg) {
	select {
	case o.feed <- m:
	default:
	}
}

// Run starts every feed goroutine. Cancel ctx to shut down; an in-flight
// supervisor turn gets a graceful SIGTERM (session stays resumable).
func (o *Orchestrator) Run(ctx context.Context) {
	go o.supervisorLoop(ctx)
	go o.watchRegistry(ctx)
	go o.pollWorkers(ctx)
	go o.ensureServers(ctx)
	for _, base := range o.endpoints() {
		go o.subscribeWorkerEvents(ctx, base)
	}
}

func (o *Orchestrator) endpoints() []string {
	seen := map[string]bool{}
	var out []string
	for _, mc := range o.Models {
		base := strings.TrimRight(mc.Endpoint, "/")
		if !seen[base] {
			seen[base] = true
			out = append(out, base)
		}
	}
	return out
}

// sessionFile persists the supervisor session id across restarts.
func (o *Orchestrator) sessionFile() string {
	return filepath.Join(o.StateDir, "supervisor-session")
}

// LoadSession returns the previously persisted supervisor session id.
func LoadSession(stateDir string) string {
	b, err := os.ReadFile(filepath.Join(stateDir, "supervisor-session"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (o *Orchestrator) supervisorLoop(ctx context.Context) {
	if sid := o.Driver.SessionID(); sid != "" {
		o.emit(ctx, ui.RawLogMsg{Source: "sup", Line: "resuming session " + sid})
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.interrupt: // nothing in flight
		case prompt := <-o.prompts:
			o.runTurn(ctx, prompt)
		}
	}
}

func (o *Orchestrator) runTurn(ctx context.Context, prompt string) {
	turn, err := o.Driver.Start(prompt)
	if err != nil {
		o.emit(ctx, ui.SupTurnDoneMsg{Err: err.Error()})
		return
	}
	pid := turn.PID()
	for {
		select {
		case <-ctx.Done():
			turn.Shutdown(3 * time.Second)
			return
		case <-o.interrupt:
			turn.Interrupt()
		case ev, ok := <-turn.Events:
			if !ok {
				return
			}
			if init, isInit := ev.(supervisor.InitEvent); isInit && init.SessionID != "" {
				if err := os.WriteFile(o.sessionFile(), []byte(init.SessionID), 0o644); err != nil {
					o.emit(ctx, ui.RawLogMsg{Source: "app", Line: "persisting session id: " + err.Error()})
				}
			}
			o.emit(ctx, mapSupEvent(ev, pid)...)
		}
	}
}

// BuildSystemPrompt tells the supervisor how to delegate: the whole reason
// the topology works. exe is the strawboss binary path.
func BuildSystemPrompt(exe string, models []config.ModelConfig) string {
	var names []string
	for _, mc := range models {
		names = append(names, fmt.Sprintf("%s (%s)", mc.Name, mc.Model))
	}
	return fmt.Sprintf(`You are a supervisor ("strawboss") that delegates self-contained coding tasks to free local AI workers instead of doing them yourself.

Delegate with the Bash tool:
  %s delegate --model <name> --task "<complete, self-contained instructions>"

Available worker models: %s. Workers run in your working directory and cannot see this conversation — every task description must stand alone. The command blocks until every worker finishes and prints one terse result per worker (worker id, status, summary, full-log path); read a log file only when you truly need detail.

Run INDEPENDENT tasks in parallel by repeating --task in ONE delegate call — each task becomes its own concurrent worker:
  %s delegate --model <m> --task "first task" --task "second task"
Only chain separate delegate calls when one task needs another's output. Do small glue work yourself; delegate anything substantial.`,
		exe, strings.Join(names, ", "), exe)
}
