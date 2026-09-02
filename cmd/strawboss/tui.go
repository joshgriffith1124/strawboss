package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/live"
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
		if m, cleanup, err = buildLive(*stateDir, *modelsPath, *fresh); err != nil {
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
	base := []string{fmt.Sprintf("Bash(%s delegate:*)", exe), "Read", "Edit", "Write", "Glob"}
	return append(base, extra...)
}

func buildLive(stateDir, modelsPath string, fresh bool) (ui.Model, func(), error) {
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
	system := cfg.Supervisor.SystemPrompt
	if system == "" {
		system = live.BuildSystemPrompt(exe, models)
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
		Command:        cfg.Supervisor.Command,
		PermissionMode: cfg.Supervisor.PermissionMode,
		AllowedTools:   allowed,
		SystemPrompt:   system,
		Dir:            cwd,
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
	o.RunID = runID
	o.Notify = cfg.Notify
	o.Budget = cfg.Budget
	o.Run(context.Background())

	m := ui.New(o.Feed())
	m.OnPrompt = o.OnPrompt
	m.OnInterrupt = o.OnInterrupt
	m.OnKillWorker = o.OnKillWorker
	m.OnRetryWorker = o.OnRetryWorker
	m.OnListSessions = o.ListSessions
	m.OnSwitchSession = o.SwitchSession
	m.OnNewSession = o.NewSession
	return m, o.Shutdown, nil
}
