package live

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// prompt is one queued user submission: the typed text plus any pasted
// images, ready to hand to the supervisor stream as one user message.
type prompt struct {
	text   string
	images []supervisor.Image
}

// Orchestrator runs the live feeds and exposes them as one tea.Msg channel.
type Orchestrator struct {
	Driver *supervisor.Driver
	// models is the current model set; read it via Models(). It is swapped
	// whole by the models.toml watcher, so goroutines never see a slice
	// mid-edit and need no lock to read it.
	models   atomic.Pointer[[]config.ModelConfig]
	StateDir string
	// ModelsPath is the models.toml the watcher follows; empty disables
	// hot reload (tests, replays).
	ModelsPath string
	// PromptFor rebuilds the supervisor system prompt for a new model set;
	// nil keeps the prompt as configured (a config.toml system_prompt
	// override is the user's text, not ours to regenerate).
	PromptFor func([]config.ModelConfig) string
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
	prompts   chan prompt
	interrupt chan struct{}
	rewind    chan struct{} // registry watcher: re-read from the top

	cancel context.CancelFunc

	// runCtx is the Run lifetime; TUI-spawned retry workers run under it
	// so Shutdown cancels them like everything else.
	runCtx context.Context

	mu              sync.Mutex
	sessionToWorker map[string]string    // session id → wN
	workerSession   map[string]string    // wN → harness session id
	workerModel     map[string]string    // wN → model config name
	workerDir       map[string]string    // wN → working directory (for scoped status)
	workerTask      map[string]string    // wN → task text (for retry)
	workerPID       map[string]int       // wN → worker subprocess pid (dsh; 0 for opencode)
	workerStarted   map[string]time.Time // wN → spawn time (reaping: predates the last boot?)
	workerDeadline  map[string]time.Time // wN → current per-attempt deadline (registry "deadline" events)
	unfinished      map[string]bool      // wN spawned but no finished event yet
	reaped          map[string]bool      // wN closed as dead by x: its finished event replays silent
	tailing         map[string]bool      // wN with a session-log tailer running (dsh)
	dshOut          map[string]int       // wN → latest output tokens seen by its tailer
	// lastRequested remembers each deadline extension the TUI itself
	// requested, so presses stack even before the registry event lands.
	lastRequested map[string]time.Time

	// ExtendStep is how much each `+` press pushes a worker's deadline
	// out; zero means the 5m default. Set from config in buildLive.
	ExtendStep time.Duration

	// RolesOn is the roles mode: whether the supervisor prompt offers
	// roles.toml's roles and pipelines. Seeded from config.toml / --roles,
	// flipped live by the TUI toggle (SetRolesMode); PromptFor reads it.
	RolesOn atomic.Bool
	// RolesPath is roles.toml, checked when the mode is switched on so a
	// missing or broken file is reported instead of silently offering
	// nothing.
	RolesPath string
	// turnBusy is true while a supervisor turn is in flight; promptStale
	// marks a system prompt changed mid-turn, applied by restarting the
	// supervisor once the turn ends. Both under mu.
	turnBusy, promptStale bool

	stream     *supervisor.Stream // the persistent supervisor process, if running
	servers    []*managedServer   // managed opencode serve children
	subscribed map[string]bool    // opencode endpoints with an event subscriber

	// budget guard accumulation (see budget.go)
	supTotals     supUsageTotals // per-run supervisor ledger (supusage.go)
	supCostTotal  float64
	fiveHourNow   float64
	budgetWarned  bool
	budgetStopped bool

	lastPrompt string // first prompt of the current session (history label)
	supModel   string // session model from system/init (picks its ctx window)

	// loud-denial tracking (see notify.go)
	toolCmds       map[string]string // toolID → "Name command…"
	deniedNotified map[string]bool   // suggestions already pushed
}

// New builds an orchestrator; call Run to start the feeds.
func New(d *supervisor.Driver, models []config.ModelConfig, stateDir string) *Orchestrator {
	o := &Orchestrator{
		Driver:          d,
		StateDir:        stateDir,
		started:         time.Now(),
		feed:            make(chan tea.Msg, 64),
		prompts:         make(chan prompt, 4),
		interrupt:       make(chan struct{}, 1),
		rewind:          make(chan struct{}, 1),
		sessionToWorker: map[string]string{},
		workerSession:   map[string]string{},
		workerModel:     map[string]string{},
		workerDir:       map[string]string{},
		workerTask:      map[string]string{},
		workerPID:       map[string]int{},
		workerStarted:   map[string]time.Time{},
		workerDeadline:  map[string]time.Time{},
		unfinished:      map[string]bool{},
		reaped:          map[string]bool{},
		tailing:         map[string]bool{},
		dshOut:          map[string]int{},
		lastRequested:   map[string]time.Time{},
		toolCmds:        map[string]string{},
		deniedNotified:  map[string]bool{},
		subscribed:      map[string]bool{},
	}
	o.models.Store(&models)
	return o
}

// Models returns the current model set. Callers that walk it more than
// once in one pass should take it once: a reload between two calls would
// hand them two different sets.
func (o *Orchestrator) Models() []config.ModelConfig {
	if p := o.models.Load(); p != nil {
		return *p
	}
	return nil
}

// Feed is the channel the UI listens on.
func (o *Orchestrator) Feed() <-chan tea.Msg { return o.feed }

// OnPrompt queues a user prompt for the supervisor (ui hook). Local
// input ends any remote conversation — replies stop relaying out.
// Pasted images (if any) ride along as attachments on the same message.
func (o *Orchestrator) OnPrompt(text string, images []ui.Image) {
	o.remoteActive.Store(false)
	supImages := make([]supervisor.Image, 0, len(images))
	for _, img := range images {
		supImages = append(supImages, supervisor.Image{MediaType: img.MediaType, Data: img.Data})
	}
	o.enqueuePrompt(prompt{text: text, images: supImages})
}

func (o *Orchestrator) enqueuePrompt(p prompt) {
	select {
	case o.prompts <- p:
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
	o.reapDead()                                          // before the watcher: close out workers a crash left "running"
	go o.emit(ctx, ui.RolesModeMsg{On: o.RolesOn.Load()}) // the configured mode, for the UI's indicator
	go o.supervisorLoop(ctx)
	go o.watchRegistry(ctx)
	go o.pollWorkers(ctx)
	go o.ensureServers(ctx)
	go o.openclawLoop(ctx)
	go o.watchModels(ctx)
	o.subscribeNewEndpoints(ctx)
}

// subscribeNewEndpoints starts an event subscriber for every opencode
// endpoint that lacks one — at startup, and again after a reload adds
// one. Subscribers for endpoints a reload removed stay up: they cost one
// idle connection, and workers already running there still report.
func (o *Orchestrator) subscribeNewEndpoints(ctx context.Context) {
	o.mu.Lock()
	var fresh []string
	for _, base := range o.endpoints() {
		if !o.subscribed[base] {
			o.subscribed[base] = true
			fresh = append(fresh, base)
		}
	}
	o.mu.Unlock()
	for _, base := range fresh {
		go o.subscribeWorkerEvents(ctx, base)
	}
}

// endpoints lists distinct opencode server base URLs. dsh model entries
// point at raw LLM endpoints — never polled, subscribed, or managed as
// opencode servers.
func (o *Orchestrator) endpoints() []string {
	seen := map[string]bool{}
	var out []string
	for _, mc := range o.Models() {
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
	for _, mc := range o.Models() {
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
				s.Interrupt() // cancels the turn in place; the process stays up
			}
		case p := <-o.prompts:
			label := p.text
			if label == "" && len(p.images) > 0 {
				label = "[image]" // images-only prompt still labels the session
			}
			o.mu.Lock()
			if o.lastPrompt == "" {
				o.lastPrompt = label // labels the session in the history
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
			if err := s.SendContent(p.text, p.images); err != nil {
				o.emit(ctx, ui.SupTurnDoneMsg{Err: err.Error()})
			} else {
				o.mu.Lock()
				o.turnBusy = true
				o.mu.Unlock()
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
		// This process's previous result: results carry running totals.
		var prevResult supervisor.ResultEvent
		for ev := range s.Events {
			if r, ok := ev.(supervisor.ResultEvent); ok {
				ev = resultDelta(r, prevResult)
				prevResult = r
			}
			switch ev.(type) {
			case supervisor.ResultEvent, supervisor.TurnDoneEvent:
				// The turn is over: a prompt change that waited for it
				// (the roles toggle mid-turn) applies now.
				o.mu.Lock()
				o.turnBusy = false
				stale := o.promptStale && o.stream == s
				if stale {
					o.promptStale = false
					o.stream = nil
				}
				o.mu.Unlock()
				if stale {
					go s.Shutdown(3 * time.Second)
				}
			}
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
			if init, isInit := ev.(supervisor.InitEvent); isInit && init.Model != "" {
				o.mu.Lock()
				o.supModel = init.Model
				o.mu.Unlock()
			}
			o.mu.Lock()
			supModel := o.supModel
			o.mu.Unlock()
			msgs := mapSupEvent(ev, pid, supModel)
			switch e := ev.(type) {
			case supervisor.InitEvent:
				// A model seen in any earlier run is known now, before a
				// single token burns — the gauge must not wait for a
				// result that an interrupted first turn may never send.
				if w := loadModelWindows(o.StateDir)[e.Model]; w > 0 {
					msgs = append(msgs, ui.SupCtxWindowMsg{Model: e.Model, Window: w})
				}
			case supervisor.ResultEvent:
				if len(e.ModelWindows) == 0 {
					break
				}
				if w := e.ModelWindows[supModel]; w > 0 {
					if err := saveModelWindow(o.StateDir, supModel, w); err != nil {
						msgs = append(msgs, ui.RawLogMsg{Source: "app", Line: err.Error()})
					}
				} else {
					// Say why the gauge stays unknown, in the logs tab,
					// instead of silently guessing.
					names := make([]string, 0, len(e.ModelWindows))
					for n := range e.ModelWindows {
						names = append(names, n)
					}
					sort.Strings(names)
					msgs = append(msgs, ui.RawLogMsg{Source: "sup",
						Line: fmt.Sprintf("context window unknown: result reports %v, session model is %q", names, supModel)})
				}
			}
			o.observeSup(msgs)
			if _, final := ev.(supervisor.TurnDoneEvent); final {
				// The turn's terminal state must reach the UI even when
				// Shutdown is what ended it: Shutdown waits for the process
				// to exit and then cancels ctx, and that cancel can win the
				// race against this goroutine's last send — the "turn
				// interrupted" message would then be dropped on the floor.
				// Bound it by time instead, so it delivers to any listener
				// and never wedges a teardown with none.
				fctx, done := context.WithTimeout(context.Background(), time.Second)
				o.emit(fctx, msgs...)
				done()
				continue
			}
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
	servers := append([]*managedServer(nil), o.servers...)
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
	for _, srv := range servers {
		if srv.cmd.Process == nil || srv.dead() {
			continue
		}
		_ = srv.cmd.Process.Signal(syscall.SIGTERM)
	}
	deadline := time.After(2 * time.Second)
	for _, srv := range servers {
		if srv.cmd.Process == nil {
			continue
		}
		select {
		case <-srv.exited:
		case <-deadline:
			_ = srv.cmd.Process.Kill()
			return
		}
	}
}

// BuildSystemPrompt tells the supervisor how to delegate: the whole reason
// the topology works. exe is the strawboss binary path.
func BuildSystemPrompt(exe string, models []config.ModelConfig) string {
	roster, retry := modelRoster(models)
	return fmt.Sprintf(`You are a supervisor ("strawboss") that delegates self-contained coding tasks to free local AI workers instead of doing them yourself.

Delegate with the Bash tool:
  %s delegate --model <name> --task "<complete, self-contained instructions>"

%sWorkers run in your working directory and cannot see this conversation — every task description must stand alone. The command blocks until every worker finishes and prints one terse result per worker (worker id, status, summary, full-log path). The log path is for the human — the TUI renders it. Never read it yourself: those files are worker transcripts and reading one costs more context than dozens of delegations, so reads under the strawboss state directory are denied. If a result is too thin to act on, delegate a follow-up task that reports what you need.

If a task's text contains $( ), backticks, or < >, do NOT put it in --task: those read as shell syntax and the whole delegate call is denied. Write the task to a file and pass the path instead — a path has no shell metacharacters, and this also suits long tasks:
  %s delegate --model <name> --task-file /tmp/task1.md
--task-file repeats and mixes with --task exactly like --task does.

Run INDEPENDENT tasks in parallel by repeating --task in ONE delegate call — each task becomes its own concurrent worker:
  %s delegate --model <m> --task "first task" --task "second task"
Only chain separate delegate calls when one task needs another's output.
A delegate call can outlast the Bash tool's default limit (a slow worker, or one the operator gives extra time from the TUI): always pass the Bash tool's maximum timeout on delegate calls so the call is never cut off mid-worker.

Delegation is not free for you: the task text you write is output tokens, the result you read comes back into your context, and the call takes 10–60s. Do it yourself when the whole job is one command you are allowed to run, or an edit you could write in fewer tokens than it takes to describe it (roughly under 30 lines) — Read, Edit and Write are yours. Delegate when a worker has to read code to work out what to write, write more than that, debug, or iterate. Never delegate "run X and report the output" for a command you can run.

Your Bash access covers:
- the delegate command above;
- read-only inspection: ls, wc, find, git status/log/diff/show (read files with Read, not cat/head/tail);
- committing: git add, git commit (use -m; for a multi-line message Write it to a file and use -F — $( ) and heredocs are denied), git switch -c / git checkout -b;
- %s stats (per-model scorecard), %s costs, %s models (which models are up, their tiers and busy slots — use this instead of probing endpoints), %s check (runs the project's verify command: a pass line, or the failing tail).
Anything else that builds, tests, installs, pushes or discards work is auto-denied. Such a denial says "Permission to use Bash has been denied", but it applies to that one command only — delegation still works, so never conclude that Bash as a whole or delegation is blocked. Read, Grep and Glob are available for looking around; delegate work that truly needs a shell.

When parallel tasks might edit the SAME files, add --worktree — including a new task that overlaps files a worker from an earlier delegate call is still editing: start it now in a worktree rather than waiting for that worker to finish. Each worker then runs in an isolated git worktree and its work is committed on its own strawboss/* branch (named in the result) instead of the shared working directory. Nothing merges automatically — tell the user which branches are waiting; they review, merge or discard them in the TUI's Worktrees tab (key b). Never merge them yourself.

Workers are SMALL local models with two hard limits. Output: about 48k tokens per reply by default (models.toml max_tokens / opencode limit.output), shared with their internal reasoning. Time: each worker is aborted after its model's timeout (20 minutes unless models.toml sets one). Parallel workers on one GPU share its throughput, so each runs slower — running several at once is the usual way to hit the time limit. Scope every task so the deliverable is modest — aim for one file of at most ~200 lines per task, never a whole app in one file. If a worker fails with "only internal reasoning", an empty reply or "context deadline exceeded", the task was too big or too slow: split it into smaller pieces or run fewer at once instead of retrying the same task.

Every worker automatically receives a compact repository map (file paths with top-level symbols), so don't spend task tokens describing the file layout — just name the files to touch. When a worker fails, delegate automatically retries the task ONCE on %s (the result says so) — never hand-retry a failed task on a different model yourself. Delegate also checks that a model is up before using it: a task aimed at one that is unreachable or not loaded runs on the nearest available model instead (the result says so), so never probe endpoints or reroute by hand. When the project defines a verify check, delegate runs it after the workers and reports "[verified: …]" or "[verify FAILED: …]" with the failing lines; a failed check counts as a failed worker. Trust that line over the worker's own claims, and act on the failing lines rather than asking a worker to re-run the check.`,
		exe, roster, exe, exe, exe, exe, exe, exe, retry)
}

// modelRoster renders the model paragraph of the supervisor prompt and the
// phrase naming where a failed task is retried. Untiered configs read as a
// flat preference list; tiered ones as a ladder, lowest tier first, each
// entry carrying the description the supervisor routes by.
func modelRoster(models []config.ModelConfig) (roster, retry string) {
	if !config.Tiered(models) {
		var names []string
		sep := ", "
		for _, mc := range models {
			name := fmt.Sprintf("%s (%s)", mc.Name, mc.Model)
			if mc.Description != "" {
				// Descriptions carry their own commas.
				name += " — " + mc.Description
				sep = "; "
			}
			names = append(names, name)
		}
		return "Available worker models, in preference order — use the first unless a task clearly needs another: " +
			strings.Join(names, sep) + ". ", "the next model config"
	}

	ladder := append([]config.ModelConfig(nil), models...)
	sort.SliceStable(ladder, func(i, j int) bool { return ladder[i].Tier < ladder[j].Tier })
	var b strings.Builder
	b.WriteString("Worker models are TIERED — tier 1 is the fastest and cheapest, higher tiers are stronger but slower:\n")
	for _, mc := range ladder {
		fmt.Fprintf(&b, "  - %s (%s) · tier %d", mc.Name, mc.Model, mc.Tier)
		if mc.Description != "" {
			b.WriteString(" — " + mc.Description)
		}
		b.WriteString("\n")
	}
	b.WriteString("Pick the LOWEST tier whose description fits the task, and size the task to that tier; when unsure pass --model auto (or omit --model) and strawboss routes each task by its weight — a failure climbs a tier on its own either way. --model is sticky: it applies to the --task flags after it, so ONE call can mix tiers:\n")
	b.WriteString("  delegate --model " + ladder[0].Name + " --task \"small task\" --model " + ladder[len(ladder)-1].Name + " --task \"big task\"\n")
	return b.String(), "the next tier up"
}
