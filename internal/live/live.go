package live

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/supervisor"
	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// Orchestrator runs the live feeds and exposes them as one tea.Msg channel.
type Orchestrator struct {
	Driver   *supervisor.Driver
	Models   []config.ModelConfig
	StateDir string
	// RunID scopes which registry events this UI shows; events stamped
	// with a different run (or none) belong to other runs and are skipped.
	RunID string
	// DshRecoverGrace is how long a dsh tailer waits after turn/end for
	// the owning delegate to record the finish before declaring the
	// worker orphaned. Default 10s; tests shorten it.
	DshRecoverGrace time.Duration
	// Notify configures optional failure pushes and OpenClaw two-way
	// remote control (docs: config.Notify).
	Notify config.Notify
	// Budget guards the metered side of the run (docs: config.Budget).
	Budget config.Budget
	// OpenClawPollEvery is the two-way poll interval. Default 5s.
	OpenClawPollEvery time.Duration

	// started marks this orchestrator's birth: replayed history from
	// before it must not re-trigger notifications.
	started time.Time
	// remoteActive: the last prompt came from the remote channel, so
	// supervisor replies relay back there until local input resumes.
	remoteActive atomic.Bool

	feed      chan tea.Msg
	prompts   chan string
	interrupt chan struct{}
	rewind    chan struct{} // registry watcher: re-read from the top

	cancel context.CancelFunc

	// runCtx is the Run lifetime; TUI-spawned retry workers run under it
	// so Shutdown cancels them like everything else.
	runCtx context.Context

	mu              sync.Mutex
	sessionToWorker map[string]string  // session id → wN
	workerSession   map[string]string  // wN → harness session id
	workerModel     map[string]string  // wN → model config name
	workerDir       map[string]string  // wN → working directory (for scoped status)
	workerTask      map[string]string  // wN → task text (for retry)
	workerPID       map[string]int     // wN → worker subprocess pid (dsh; 0 for opencode)
	unfinished      map[string]bool    // wN spawned but no finished event yet
	tailing         map[string]bool    // wN with a session-log tailer running (dsh)
	dshOut          map[string]int     // wN → latest output tokens seen by its tailer
	stream          *supervisor.Stream // the persistent supervisor process, if running
	servers         []*exec.Cmd        // managed opencode serve children

	// budget guard accumulation (see budget.go)
	supTotals     supUsageTotals // per-run supervisor ledger (supusage.go)
	supCostTotal  float64
	fiveHourNow   float64
	budgetWarned  bool
	budgetStopped bool

	lastPrompt string // first prompt of the current session (history label)

	// loud-denial tracking (see notify.go)
	toolCmds       map[string]string // toolID → "Name command…"
	deniedNotified map[string]bool   // suggestions already pushed
}

// New builds an orchestrator; call Run to start the feeds.
func New(d *supervisor.Driver, models []config.ModelConfig, stateDir string) *Orchestrator {
	return &Orchestrator{
		Driver:          d,
		Models:          models,
		StateDir:        stateDir,
		started:         time.Now(),
		feed:            make(chan tea.Msg, 64),
		prompts:         make(chan string, 4),
		interrupt:       make(chan struct{}, 1),
		rewind:          make(chan struct{}, 1),
		sessionToWorker: map[string]string{},
		workerSession:   map[string]string{},
		workerModel:     map[string]string{},
		workerDir:       map[string]string{},
		workerTask:      map[string]string{},
		workerPID:       map[string]int{},
		unfinished:      map[string]bool{},
		tailing:         map[string]bool{},
		dshOut:          map[string]int{},
		toolCmds:        map[string]string{},
		deniedNotified:  map[string]bool{},
	}
}

// Feed is the channel the UI listens on.
func (o *Orchestrator) Feed() <-chan tea.Msg { return o.feed }

// OnPrompt queues a user prompt for the supervisor (ui hook). Local
// input ends any remote conversation — replies stop relaying out.
func (o *Orchestrator) OnPrompt(text string) {
	o.remoteActive.Store(false)
	o.enqueuePrompt(text)
}

func (o *Orchestrator) enqueuePrompt(text string) {
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

// Run starts every feed goroutine. Call Shutdown when the UI exits.
func (o *Orchestrator) Run(ctx context.Context) {
	ctx, o.cancel = context.WithCancel(ctx)
	o.runCtx = ctx
	o.seedSupUsage()
	go o.supervisorLoop(ctx)
	go o.watchRegistry(ctx)
	go o.pollWorkers(ctx)
	go o.ensureServers(ctx)
	go o.openclawLoop(ctx)
	for _, base := range o.endpoints() {
		go o.subscribeWorkerEvents(ctx, base)
	}
}

// endpoints lists distinct opencode server base URLs. dsh model entries
// point at raw LLM endpoints — never polled, subscribed, or managed as
// opencode servers.
func (o *Orchestrator) endpoints() []string {
	seen := map[string]bool{}
	var out []string
	for _, mc := range o.Models {
		if mc.Harness != "" && mc.Harness != "opencode" {
			continue
		}
		base := strings.TrimRight(mc.Endpoint, "/")
		if !seen[base] {
			seen[base] = true
			out = append(out, base)
		}
	}
	return out
}

// modelConfig resolves a model config by name.
func (o *Orchestrator) modelConfig(name string) (config.ModelConfig, bool) {
	for _, mc := range o.Models {
		if mc.Name == name {
			return mc, true
		}
	}
	return config.ModelConfig{}, false
}

// Session and run persistence is scoped PER PROJECT DIRECTORY — a global
// pointer would make strawboss launched in project B resume project A's
// supervisor, old context and all. Each cwd gets its own slot
// under <stateDir>/projects/<hash>/; a `dir` file inside names the path
// for humans. Legacy global supervisor-session/run files are ignored.

// ProjectDir is the state slot for one working directory (exported: the
// delegate command reads the budget-stop marker from it).
func ProjectDir(stateDir, dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	return filepath.Join(stateDir, "projects", hex.EncodeToString(sum[:6]))
}

// sessionFile persists the supervisor session id across restarts.
func (o *Orchestrator) sessionFile() string {
	return filepath.Join(ProjectDir(o.StateDir, o.Driver.Dir), "supervisor-session")
}

// LoadSession returns the supervisor session id last persisted for this
// working directory.
func LoadSession(stateDir, dir string) string {
	b, err := os.ReadFile(filepath.Join(ProjectDir(stateDir, dir), "supervisor-session"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// RunID returns the persisted run id scoping registry events to this
// working directory's supervisor run; rotate (or a missing file) mints a
// fresh one, so a new session starts with an empty worker table while a
// resumed session replays its own workers.
func RunID(stateDir, dir string, rotate bool) (string, error) {
	slot := ProjectDir(stateDir, dir)
	path := filepath.Join(slot, "run")
	if !rotate {
		if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return strings.TrimSpace(string(b)), nil
		}
	}
	id := fmt.Sprintf("run-%d", time.Now().UnixNano())
	if err := os.MkdirAll(slot, 0o755); err != nil {
		return "", fmt.Errorf("minting run id: %w", err)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		_ = os.WriteFile(filepath.Join(slot, "dir"), []byte(abs+"\n"), 0o644)
	}
	if err := os.WriteFile(path, []byte(id), 0o644); err != nil {
		return "", fmt.Errorf("minting run id: %w", err)
	}
	return id, nil
}

// supervisorLoop owns one persistent supervisor process (spawned lazily on
// the first prompt, respawned with --resume after a crash or interrupt).
// Prompts are injected over stdin at ANY time — including mid-turn, so the
// supervisor is never deaf while workers run.
func (o *Orchestrator) supervisorLoop(ctx context.Context) {
	if sid := o.Driver.SessionID(); sid != "" {
		o.emit(ctx, ui.RawLogMsg{Source: "sup", Line: "resuming session " + sid})
	}
	current := func() *supervisor.Stream {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.stream
	}
	for {
		select {
		case <-ctx.Done():
			if s := current(); s != nil {
				s.Shutdown(3 * time.Second)
			}
			return
		case <-o.interrupt:
			if s := current(); s != nil && s.Alive() {
				s.Interrupt() // process exits; next prompt respawns with --resume
			}
		case prompt := <-o.prompts:
			o.mu.Lock()
			if o.lastPrompt == "" {
				o.lastPrompt = prompt // labels the session in the history
			}
			o.mu.Unlock()
			s := current()
			if s == nil || !s.Alive() {
				var err error
				if s, err = o.startStream(ctx); err != nil {
					o.emit(ctx, ui.SupTurnDoneMsg{Err: err.Error()})
					continue
				}
			}
			if err := s.Send(prompt); err != nil {
				o.emit(ctx, ui.SupTurnDoneMsg{Err: err.Error()})
			}
		}
	}
}

// startStream spawns the persistent supervisor and pumps its events.
func (o *Orchestrator) startStream(ctx context.Context) (*supervisor.Stream, error) {
	s, err := o.Driver.StartStream()
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	o.stream = s
	o.mu.Unlock()
	go func() {
		pid := s.PID()
		for ev := range s.Events {
			if init, isInit := ev.(supervisor.InitEvent); isInit && init.SessionID != "" {
				_ = os.MkdirAll(filepath.Dir(o.sessionFile()), 0o755)
				if err := os.WriteFile(o.sessionFile(), []byte(init.SessionID), 0o644); err != nil {
					o.emit(ctx, ui.RawLogMsg{Source: "app", Line: "persisting session id: " + err.Error()})
				}
				o.mu.Lock()
				run, label := o.RunID, o.lastPrompt
				o.mu.Unlock()
				o.appendSessionHistory(init.SessionID, run, label)
			}
			msgs := mapSupEvent(ev, pid)
			o.observeSup(msgs)
			o.emit(ctx, msgs...)
		}
	}()
	return s, nil
}

// Shutdown tears the whole operation down when the UI exits: abort every
// unfinished worker (nothing keeps running unobserved), SIGTERM the
// in-flight supervisor turn (session stays resumable), stop managed
// opencode servers, and cancel the feed goroutines.
func (o *Orchestrator) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	type target struct {
		session, model string
		pid            int
	}
	o.mu.Lock()
	var workers []target
	for wid := range o.unfinished {
		workers = append(workers, target{o.workerSession[wid], o.workerModel[wid], o.workerPID[wid]})
	}
	stream := o.stream
	servers := append([]*exec.Cmd(nil), o.servers...)
	o.mu.Unlock()

	// Abort workers first, while any managed server is still up. dsh
	// workers are subprocesses of their delegate — SIGTERM the recorded
	// pid; opencode workers are server sessions — abort via the API.
	for _, w := range workers {
		if mc, ok := o.modelConfig(w.model); ok && mc.Harness == "dsh" {
			if w.pid > 0 {
				_ = syscall.Kill(w.pid, syscall.SIGTERM)
			}
			continue
		}
		if c := o.clientFor(w.model); c != nil {
			_ = c.Abort(ctx, w.session)
		}
	}
	if stream != nil {
		stream.Shutdown(3 * time.Second)
	}
	if o.cancel != nil {
		o.cancel()
	}
	for _, cmd := range servers {
		if cmd.Process == nil || cmd.ProcessState != nil {
			continue
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	deadline := time.After(2 * time.Second)
	for _, cmd := range servers {
		if cmd.Process == nil {
			continue
		}
		for cmd.ProcessState == nil {
			select {
			case <-deadline:
				_ = cmd.Process.Kill()
				return
			case <-time.After(50 * time.Millisecond):
			}
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

Available worker models, in preference order — use the first unless a task clearly needs another: %s. Workers run in your working directory and cannot see this conversation — every task description must stand alone. The command blocks until every worker finishes and prints one terse result per worker (worker id, status, summary, full-log path); read a log file only when you truly need detail.

Run INDEPENDENT tasks in parallel by repeating --task in ONE delegate call — each task becomes its own concurrent worker:
  %s delegate --model <m> --task "first task" --task "second task"
Only chain separate delegate calls when one task needs another's output. Do small glue work yourself (you may Read, Edit, and Write files directly); delegate anything substantial.

Your Bash access covers ONLY the delegate command above; any other shell command (git, ls, builds, tests) is auto-denied. Such a denial says "Permission to use Bash has been denied", but it applies to that one command only — delegation still works, so never conclude that Bash as a whole or delegation is blocked. Look around with Read and Glob instead, and delegate work that truly needs a shell.

When parallel tasks might edit the SAME files, add --worktree: each worker then runs in an isolated git worktree and its work is committed on its own strawboss/* branch (named in the result) instead of the shared working directory. Nothing merges automatically — relay the branch names to the user for review.

Workers are SMALL local models with a limited output budget (roughly 16k tokens, shared with their internal reasoning). Scope every task so the deliverable is modest — aim for one file of at most ~200 lines per task, never a whole app in one file. If a worker fails with "only internal reasoning" or an empty reply, the task was too big: split it into smaller pieces instead of retrying the same task.

Every worker automatically receives a compact repository map (file paths with top-level symbols), so don't spend task tokens describing the file layout — just name the files to touch. When a worker fails, delegate automatically retries the task ONCE on the next model config (the result says so) — never hand-retry a failed task on a different model yourself.`,
		exe, strings.Join(names, ", "), exe)
}
