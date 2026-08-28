package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"strawboss/internal/config"
	"strawboss/internal/harness"
	"strawboss/internal/harness/opencode"
	"strawboss/internal/registry"
)

// runDelegate is the command the supervisor calls to spawn a worker. Its
// stdout is the ONLY thing that enters supervisor context (invariant 3):
// worker id, status, a few-line summary, and the full-log path — target
// ≤ ~250 tokens. Everything else (live transcript, registry events) flows
// to the TUI locally.
func runDelegate(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("delegate", flag.ContinueOnError)
	model := fs.String("model", "", "model config name from models.toml (required)")
	task := fs.String("task", "", "task prompt for the worker (required)")
	dir := fs.String("dir", "", "worker working directory (default: current directory)")
	timeout := fs.Duration("timeout", 20*time.Minute, "abort the worker after this long")
	stateDir := fs.String("state-dir", "", "state directory (default ~/.strawboss)")
	modelsPath := fs.String("models", "", "models.toml path (default <state-dir>/models.toml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *model == "" || *task == "" {
		return errors.New("delegate: --model and --task are required")
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
	if *dir == "" {
		var err error
		if *dir, err = os.Getwd(); err != nil {
			return fmt.Errorf("delegate: %w", err)
		}
	}

	models, err := config.LoadModels(*modelsPath)
	if err != nil {
		return err
	}
	var mc config.ModelConfig
	found := false
	for _, m := range models {
		if m.Name == *model {
			mc, found = m, true
			break
		}
	}
	if !found {
		return fmt.Errorf("delegate: no model %q in %s", *model, *modelsPath)
	}

	// mc.Harness is validated by LoadModels; opencode is the only v1 value.
	h := opencode.New(mc, *dir, filepath.Join(*stateDir, "logs"))
	reg := &registry.Registry{Path: filepath.Join(*stateDir, "workers.jsonl")}

	// Timeout and supervisor interrupt (SIGINT/SIGTERM when the Bash call
	// is killed) both cancel the wait; the worker is then aborted rather
	// than left running unobserved.
	ctx, cancelTimeout := context.WithTimeout(context.Background(), *timeout)
	defer cancelTimeout()
	ctx, cancelSignals := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancelSignals()

	start := time.Now()
	session, err := h.Spawn(ctx, *task, mc)
	if err != nil {
		return fmt.Errorf("delegate: %w", err)
	}
	wid, err := reg.Allocate(session, mc.Name, *task, *dir)
	if err != nil {
		return fmt.Errorf("delegate: %w", err)
	}

	res, err := h.Result(ctx, session)
	if err != nil {
		if ctx.Err() == nil {
			return fmt.Errorf("delegate: %w", err)
		}
		// Timed out or interrupted: abort the worker, report failed.
		abortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if aerr := h.Abort(abortCtx, session); aerr != nil {
			fmt.Fprintf(os.Stderr, "strawboss: %v\n", aerr)
		}
		res = harness.Result{
			WorkerID: session,
			Status:   harness.StatusFailed,
			Summary:  fmt.Sprintf("aborted: %v after %s", context.Cause(ctx), time.Since(start).Round(time.Second)),
		}
	}

	var usage harness.Usage
	if u, uerr := h.Usage(context.Background(), session); uerr == nil {
		usage = u
	}
	if err := reg.Finish(wid, session, string(res.Status), res.Summary, res.LogPath,
		time.Since(start), usage.InputTokens, usage.OutputTokens); err != nil {
		fmt.Fprintf(os.Stderr, "strawboss: %v\n", err)
	}

	// The terse result. Keep this format stable: the TUI pairs the
	// supervisor's tool_result against it, and the supervisor reads it.
	fmt.Fprintf(stdout, "%s %s %s · log %s\n%s\n",
		wid, res.Status, time.Since(start).Round(time.Second), res.LogPath, res.Summary)

	if res.Status != harness.StatusDone {
		return fmt.Errorf("worker %s %s", wid, res.Status)
	}
	return nil
}
