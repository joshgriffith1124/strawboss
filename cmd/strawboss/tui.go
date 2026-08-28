package main

import (
	"flag"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"strawboss/internal/ui"
	"strawboss/internal/ui/replay"
)

// runTUI launches the Bubble Tea app. Until M5 wires the live feeds, the
// only source is the demo replay (recorded streams).
func runTUI(args []string) error {
	fs := flag.NewFlagSet("strawboss", flag.ContinueOnError)
	demo := fs.Bool("demo", true, "drive the UI from recorded streams (live mode lands in M5)")
	speed := fs.Float64("speed", 1, "demo replay speed multiplier")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*demo {
		return fmt.Errorf("live mode not implemented yet (M5) — run without flags for the demo replay")
	}

	m := ui.New(replay.Feed(*speed))
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("running TUI: %w", err)
	}
	return nil
}
