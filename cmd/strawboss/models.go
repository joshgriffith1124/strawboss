package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/live"
	"github.com/joshgriffith1124/strawboss/internal/slots"
)

// runModels answers "which of my models can take a task right now?" in a
// few dozen tokens instead of delegating an LLM worker to find out: one
// line per model in models.toml order, probed concurrently.
func runModels(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "state directory (default ~/.strawboss)")
	modelsPath := fs.String("models", "", "models.toml path (default <state-dir>/models.toml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *stateDir == "" {
		var err error
		if *stateDir, err = config.DefaultStateDir(); err != nil {
			return err
		}
	}
	if *modelsPath == "" {
		*modelsPath = filepath.Join(*stateDir, "models.toml")
	}
	models, err := config.LoadModels(*modelsPath)
	if err != nil {
		return err
	}

	// Probe every model in its own goroutine; the cap keeps one dead
	// endpoint from dragging the whole table with it.
	type probe struct {
		ok  bool
		why string
	}
	probes := make([]probe, len(models))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i, mc := range models {
		wg.Add(1)
		go func(i int, mc config.ModelConfig) {
			defer wg.Done()
			ok, why := live.Available(ctx, mc)
			probes[i] = probe{ok: ok, why: why}
		}(i, mc)
	}
	wg.Wait()

	tiered := config.Tiered(models)
	nameW := 0
	for _, mc := range models {
		if len(mc.Name) > nameW {
			nameW = len(mc.Name)
		}
	}
	slotDir := filepath.Join(*stateDir, "slots")
	for i, mc := range models {
		p := probes[i]
		line := []string{fmt.Sprintf("%-*s", nameW, mc.Name)}
		if tiered {
			line = append(line, fmt.Sprintf("tier %d", mc.Tier))
		}
		if p.ok {
			line = append(line, "up")
		} else {
			line = append(line, fmt.Sprintf("DOWN (%s)", p.why))
		}
		if mc.MaxConcurrent > 0 {
			line = append(line, fmt.Sprintf("slots %d/%d busy", slots.Busy(slotDir, mc.Name, mc.MaxConcurrent), mc.MaxConcurrent))
		}
		line = append(line, mc.Model)
		fmt.Fprintln(stdout, strings.Join(line, "  "))
	}
	return nil
}
