package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/clipboard"
	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/herdr"
	"github.com/joshgriffith1124/strawboss/internal/live"
	"github.com/joshgriffith1124/strawboss/internal/registry"
	"github.com/joshgriffith1124/strawboss/internal/supervisor"
	"github.com/joshgriffith1124/strawboss/internal/ui"
	"github.com/joshgriffith1124/strawboss/internal/ui/replay"
)

// runTUI launches the Bubble Tea app over the live feeds (supervisor
// driver + registry watcher + opencode polling), or the recorded-stream
// replay with --demo.
func runTUI(args []string) error {
	fs := flag.NewFlagSet("strawboss", flag.ContinueOnError)
	demo := fs.Bool("demo", false, "drive the UI from recorded streams instead of live feeds")
	speed := fs.Float64("speed", 1, "demo replay speed multiplier")
	fresh := fs.Bool("new", false, "start a fresh supervisor session instead of resuming the last one")
	rolesMode := fs.String("roles", "", "roles mode for this run: on or off (default: config.toml [roles] mode, else off)")
	stateDir := fs.String("state-dir", "", "state directory (default ~/.strawboss)")
	modelsPath := fs.String("models", "", "models.toml path (default <state-dir>/models.toml)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Capture stderr before the alt screen goes up: a crash's traceback is
	// otherwise written over a screen the TUI is about to restore, and is
	// lost with the scrollback. Non-fatal if it fails — running without a
	// crash log beats not running.
	sd := *stateDir
	if sd == "" {
		if d, err := config.DefaultStateDir(); err == nil {
			sd = d
		}
	}
	if sd != "" {
		if err := os.MkdirAll(sd, 0o755); err == nil {
			if closeCrashLog, err := captureStderr(sd); err == nil {
				defer closeCrashLog()
			}
		}
	}

	var m ui.Model
	cleanup := func() {}
	if *demo {
		m = ui.New(replay.Feed(*speed))
	} else {
		var err error
		if m, cleanup, err = buildLive(*stateDir, *modelsPath, *fresh, *rolesMode); err != nil {
			return err
		}
	}
	// Exiting must kill the whole operation: active workers are aborted,
	// the supervisor turn is terminated (resumable), managed opencode
	// servers stop. Nothing keeps running unobserved.
	defer cleanup()

	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("running TUI: %w", err)
	}
	return nil
}

// supervisorAllowedTools builds the --allowedTools list: a fixed baseline
// plus any config'd extras. The baseline is never replaced — losing the
// delegate pattern to a config edit would silently break the whole
// topology. Read/Edit/Write cover the "small glue work" the system prompt
// asks for — without them the supervisor gets denied mid-repair. Glob is
// the sanctioned way to look around a directory: in dontAsk mode a bare
// `ls` is denied.
func supervisorAllowedTools(exe string, extra []string) []string {
	base := []string{
		fmt.Sprintf("Bash(%s delegate:*)", exe),
		// Small glue work the system prompt asks for.
		"Read", "Edit", "Write", "Glob",
		// Grep was missing and was the single most-denied tool (19% of
		// its calls, in every project) — it belongs with Read and Glob,
		// which were always allowed. See docs/NOTES.md.
		"Grep",
		// Research the supervisor would otherwise be denied outright.
		"WebFetch", "WebSearch",
		// Read-only shell. Every one of these appeared in a real denial;
		// nothing here writes, so the supervisor can look around without
		// gaining the ability to act outside the delegate contract.
		//
		// cat/head/tail are deliberately absent even though they were
		// denied too: file content comes through Read, which honours the
		// path deny rules below. A Bash rule cannot be path-scoped, so
		// granting cat would reopen the worker-transcript hole that
		// supervisorDisallowedTools exists to close. ls/wc/find leak only
		// names and counts, which is why they stay.
		"Bash(ls:*)", "Bash(wc:*)", "Bash(find:*)",
		"Bash(git status:*)", "Bash(git log:*)", "Bash(git diff:*)", "Bash(git show:*)",
		// Bounded writes whose output is a line or two. Without them the
		// supervisor had to delegate an LLM worker for a one-line commit —
		// ~300-600 supervisor tokens and 10-30s for what costs ~50 tokens
		// direct (docs/NOTES.md). Nothing here rewrites history, discards
		// work or leaves the machine: reset, checkout of paths, clean and
		// push stay denied.
		"Bash(git add:*)", "Bash(git commit:*)",
		"Bash(git switch -c:*)", "Bash(git checkout -b:*)",
		// strawboss's own read-only answers: short by construction, so the
		// supervisor asks them instead of delegating a worker to find out.
		fmt.Sprintf("Bash(%s stats:*)", exe),
		fmt.Sprintf("Bash(%s costs:*)", exe),
		fmt.Sprintf("Bash(%s models:*)", exe),
		fmt.Sprintf("Bash(%s check:*)", exe),
	}
	return append(base, extra...)
}

// supervisorDisallowedTools keeps the state directory out of supervisor
// context. Worker transcripts, dsh session logs and the registry all live
// there, and invariant 3 says they reach the TUI and never the
// supervisor: an audit found 16 such reads costing ~35,300 tokens against
// delegate results averaging 181 (docs/NOTES.md). Read is broadly
// allowed, so only a deny rule can carve this hole out of it.
//
// The //abs form is required. A plain absolute path in a Read() rule is
// treated as relative to cwd and silently matches nothing — verified
// against the real CLI, both ways.
func supervisorDisallowedTools(stateDir string) []string {
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return nil // no rule beats a rule that silently matches nothing
	}
	return []string{"Read(//" + strings.TrimPrefix(filepath.ToSlash(abs), "/") + "/**)"}
}

func buildLive(stateDir, modelsPath string, fresh bool, rolesMode string) (ui.Model, func(), error) {
	var zero ui.Model
	if stateDir == "" {
		var err error
		if stateDir, err = config.DefaultStateDir(); err != nil {
			return zero, nil, err
		}
	}
	cfg, err := config.Load(filepath.Join(stateDir, "config.toml"))
	if err != nil {
		return zero, nil, err
	}
	if modelsPath == "" {
		modelsPath = filepath.Join(stateDir, "models.toml")
	}
	models, err := config.LoadModels(modelsPath)
	if err != nil {
		return zero, nil, fmt.Errorf("%w\n(create it from examples/models.toml — workers need model configs)", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return zero, nil, fmt.Errorf("resolving strawboss binary: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return zero, nil, fmt.Errorf("resolving cwd: %w", err)
	}

	allowed := supervisorAllowedTools(exe, cfg.Supervisor.AllowedTools)
	denied := supervisorDisallowedTools(stateDir)
	// Roles mode (opt-in): config.toml [roles] mode, overridden per run by
	// --roles. On, the supervisor is offered roles.toml's roles and
	// pipelines; off, its prompt never mentions them.
	rolesOn := cfg.Roles.On()
	switch rolesMode {
	case "on":
		rolesOn = true
	case "off":
		rolesOn = false
	case "":
	default:
		return zero, nil, fmt.Errorf("--roles %q: want on or off", rolesMode)
	}
	rolesPath := filepath.Join(stateDir, "roles.toml")
	if rolesOn {
		if _, err := config.LoadRoles(rolesPath, models); err != nil {
			return zero, nil, err
		}
	}
	buildPrompt := func(ms []config.ModelConfig, withRoles bool) string {
		p := live.BuildSystemPrompt(exe, ms)
		if withRoles {
			// A roles.toml broken by a later edit drops the section rather
			// than the whole prompt (the toggle toasts the error).
			if roles, err := config.LoadRoles(rolesPath, ms); err == nil {
				p += live.RolesPrompt(exe, roles)
			}
		}
		return p
	}
	system := cfg.Supervisor.SystemPrompt
	if system == "" {
		system = buildPrompt(models, rolesOn)
	}
	// The run id scopes worker history: --new rotates it so old runs'
	// workers don't replay into a fresh session; resume keeps it. Both it
	// and the session pointer are per working directory — strawboss in a
	// new project must never resume another project's supervisor.
	runID, err := live.RunID(stateDir, cwd, fresh)
	if err != nil {
		return zero, nil, err
	}
	driver := &supervisor.Driver{
		Command:         cfg.Supervisor.Command,
		PermissionMode:  cfg.Supervisor.PermissionMode,
		AllowedTools:    allowed,
		DisallowedTools: denied,
		SystemPrompt:    system,
		Dir:             cwd,
		// Inherited by delegate (via the supervisor's Bash) to stamp
		// registry events with this run.
		Env: []string{"STRAWBOSS_RUN=" + runID},
	}
	if !fresh {
		if sid := live.LoadSession(stateDir, cwd); sid != "" {
			driver.SetSessionID(sid)
		}
	}

	// A fresh run (or a disabled budget) starts unblocked; a window-based
	// stop from a resumed run re-evaluates live anyway.
	if fresh || (cfg.Budget.MaxCostUSD <= 0 && cfg.Budget.MaxPlan5h <= 0) {
		_ = os.Remove(live.BudgetStopFile(stateDir, cwd))
	}

	o := live.New(driver, models, stateDir)
	o.ModelsPath = modelsPath
	if cfg.Supervisor.SystemPrompt == "" {
		// Reads the live roles mode, so the ctrl+r toggle and a models.toml
		// reload both rebuild the prompt the same way.
		o.PromptFor = func(ms []config.ModelConfig) string { return buildPrompt(ms, o.RolesOn.Load()) }
	}
	o.RolesOn.Store(rolesOn)
	o.RolesPath = rolesPath
	o.RunID = runID
	o.Notify = cfg.Notify
	o.Budget = cfg.Budget
	o.ExtendStep = cfg.Workers.ExtendStep
	o.Run(context.Background())

	// Inside a herdr pane, mirror the feed to herdr's agent sidebar. It
	// only reads what the UI already gets — no supervisor tokens.
	feed, shutdown := o.Feed(), o.Shutdown
	if hc := herdr.FromEnv(); hc != nil {
		var stopHerdr func()
		feed, stopHerdr = herdr.Tee(hc, feed)
		shutdown = func() { o.Shutdown(); stopHerdr() }
	}

	m := ui.New(feed)
	m.OnPrompt = o.OnPrompt
	m.OnInterrupt = o.OnInterrupt
	// Clipboard paste: read + fit off the Update loop; the result comes
	// back as a ui.ImagePastedMsg. A missing clipboard tool or empty
	// image becomes a toast, never a crash.
	m.OnPasteImage = func() (ui.Image, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		data, err := clipboard.ReadImage(ctx)
		if errors.Is(err, clipboard.ErrNoImage) {
			return ui.Image{}, ui.ErrNoImage // lets ctrl+v fall back to text paste
		}
		if err != nil {
			return ui.Image{}, err
		}
		f, err := clipboard.Fit(data)
		if err != nil {
			return ui.Image{}, err
		}
		return ui.Image{MediaType: f.MediaType, Data: f.Data, Width: f.Width, Height: f.Height}, nil
	}
	m.OnKillWorker = o.OnKillWorker
	m.OnRetryWorker = o.OnRetryWorker
	m.OnExtendWorker = o.OnExtendWorker
	m.OnToggleRoles = o.SetRolesMode
	m.OnListSessions = o.ListSessions
	m.OnSwitchSession = o.SwitchSession
	m.OnNewSession = o.NewSession
	m.OnListBranches = o.ListBranches
	m.OnBranchDiff = o.BranchDiff
	m.OnMergeBranch = o.MergeBranch
	m.OnDiscardBranch = o.DiscardBranch
	m.OnUndoWorker = o.UndoWorker
	// Analytics tab: reload the all-runs per-model scorecard on demand,
	// the same way `strawboss stats` does. A missing or corrupt registry
	// is a displayed empty state, never a crash.
	m.OnScorecard = func() []ui.ScoreRow {
		reg := &registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}
		events, err := reg.Load()
		if err != nil {
			return nil
		}
		scores := scorecard(events, "", time.Time{})
		rows := make([]ui.ScoreRow, 0, len(scores))
		for _, s := range scores {
			rows = append(rows, ui.ScoreRow{Model: s.Model, Attempts: s.Attempts,
				Done: s.Done, Failed: s.Failed, Rescued: s.Rescued, Median: s.Median})
		}
		return rows
	}
	return m, shutdown, nil
}
