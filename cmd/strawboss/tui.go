package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"strawboss/internal/config"
	"strawboss/internal/live"
	"strawboss/internal/supervisor"
	"strawboss/internal/ui"
	"strawboss/internal/ui/replay"
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

	var m ui.Model
	if *demo {
		m = ui.New(replay.Feed(*speed))
	} else {
		var err error
		if m, err = buildLive(*stateDir, *modelsPath, *fresh); err != nil {
			return err
		}
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("running TUI: %w", err)
	}
	return nil
}

func buildLive(stateDir, modelsPath string, fresh bool) (ui.Model, error) {
	var zero ui.Model
	if stateDir == "" {
		var err error
		if stateDir, err = config.DefaultStateDir(); err != nil {
			return zero, err
		}
	}
	cfg, err := config.Load(filepath.Join(stateDir, "config.toml"))
	if err != nil {
		return zero, err
	}
	if modelsPath == "" {
		modelsPath = filepath.Join(stateDir, "models.toml")
	}
	models, err := config.LoadModels(modelsPath)
	if err != nil {
		return zero, fmt.Errorf("%w\n(create it from examples/models.toml — workers need model configs)", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return zero, fmt.Errorf("resolving strawboss binary: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return zero, fmt.Errorf("resolving cwd: %w", err)
	}

	allowed := cfg.Supervisor.AllowedTools
	if len(allowed) == 0 {
		allowed = []string{fmt.Sprintf("Bash(%s delegate:*)", exe), "Read"}
	}
	system := cfg.Supervisor.SystemPrompt
	if system == "" {
		system = live.BuildSystemPrompt(exe, models)
	}
	driver := &supervisor.Driver{
		Command:        cfg.Supervisor.Command,
		PermissionMode: cfg.Supervisor.PermissionMode,
		AllowedTools:   allowed,
		SystemPrompt:   system,
		Dir:            cwd,
	}
	if !fresh {
		if sid := live.LoadSession(stateDir); sid != "" {
			driver.SetSessionID(sid)
		}
	}

	o := live.New(driver, models, stateDir)
	o.Run(context.Background())

	m := ui.New(o.Feed())
	m.OnPrompt = o.OnPrompt
	m.OnInterrupt = o.OnInterrupt
	return m, nil
}
