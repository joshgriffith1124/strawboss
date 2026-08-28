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
	"sync"
	"syscall"
	"time"

	"strawboss/internal/config"
	"strawboss/internal/harness"
	"strawboss/internal/harness/opencode"
	"strawboss/internal/registry"
)

// runDelegate is the command the supervisor calls to spawn workers. Its
// stdout is the ONLY thing that enters supervisor context (invariant 3):
// per worker, one header line and a few-line summary — target ≤ ~250
// tokens each. Everything else (live transcripts, registry events) flows
// to the TUI locally.
//
// --task may repeat: each task becomes its own worker and they all run in
// parallel inside this one invocation (a single Bash call on the
// supervisor side — compound shell commands don't pass the allowlist).
func runDelegate(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("delegate", flag.ContinueOnError)
	model := fs.String("model", "", "model config name from models.toml (required)")
	var tasks []string
	fs.Func("task", "task prompt for a worker (repeat for parallel workers)", func(s string) error {
		tasks = append(tasks, s)
		return nil
	})
	dir := fs.String("dir", "", "worker working directory (default: current directory)")
	timeout := fs.Duration("timeout", 20*time.Minute, "abort workers after this long")
	stateDir := fs.String("state-dir", "", "state directory (default ~/.strawboss)")
	modelsPath := fs.String("models", "", "models.toml path (default <state-dir>/models.toml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *model == "" || len(tasks) == 0 {
		return errors.New("delegate: --model and at least one --task are required")
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
	// STRAWBOSS_RUN is set by the TUI on the supervisor subprocess and
	// inherited here; it scopes these workers to that run.
	reg := &registry.Registry{Path: filepath.Join(*stateDir, "workers.jsonl"), Run: os.Getenv("STRAWBOSS_RUN")}

	// Timeout and supervisor interrupt (SIGINT/SIGTERM when the Bash call
	// is killed) both cancel the wait; workers are then aborted rather
	// than left running unobserved.
	ctx, cancelTimeout := context.WithTimeout(context.Background(), *timeout)
	defer cancelTimeout()
	ctx, cancelSignals := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancelSignals()

	outcomes := make([]workerOutcome, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcomes[i] = runWorker(ctx, h, reg, mc, task, *dir)
		}()
	}
	wg.Wait()

	failed := 0
	for i, oc := range outcomes {
		if oc.err != nil {
			failed++
			fmt.Fprintf(stdout, "task %d error: %v\n", i+1, oc.err)
			continue
		}
		// The terse result. Keep this format stable: the TUI pairs the
		// supervisor's tool_result against it, and the supervisor reads it.
		fmt.Fprintf(stdout, "%s %s %s · log %s\n%s\n",
			oc.wid, oc.res.Status, oc.dur.Round(time.Second), oc.res.LogPath, oc.res.Summary)
		if oc.res.Status != harness.StatusDone {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d workers failed", failed, len(tasks))
	}
	return nil
}

type workerOutcome struct {
	wid string
	res harness.Result
	dur time.Duration
	err error
}

// runWorker runs one task to completion: spawn, register, wait, record.
func runWorker(ctx context.Context, h *opencode.Harness, reg *registry.Registry, mc config.ModelConfig, task, dir string) (oc workerOutcome) {
	start := time.Now()
	session, err := h.Spawn(ctx, task, mc)
	if err != nil {
		oc.err = err
		return oc
	}
	oc.wid, err = reg.Allocate(session, mc.Name, task, dir)
	if err != nil {
		oc.err = err
		return oc
	}

	res, err := h.Result(ctx, session)
	if err != nil {
		if ctx.Err() == nil {
			oc.err = err
			return oc
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
	oc.res = res
	oc.dur = time.Since(start)

	var usage harness.Usage
	if u, uerr := h.Usage(context.Background(), session); uerr == nil {
		usage = u
	}
	if err := reg.Finish(oc.wid, session, string(res.Status), res.Summary, res.LogPath,
		oc.dur, usage.InputTokens, usage.OutputTokens); err != nil {
		fmt.Fprintf(os.Stderr, "strawboss: %v\n", err)
	}
	return oc
}
