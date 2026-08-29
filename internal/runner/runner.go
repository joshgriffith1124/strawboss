// Package runner executes one worker task to completion: spawn, register,
// wait, record. It is shared by the delegate command (supervisor-initiated)
// and the TUI's manual retry (user-initiated) so both paths register and
// finish workers identically, and it owns harness selection by model
// config.
package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"strawboss/internal/config"
	"strawboss/internal/harness"
	"strawboss/internal/harness/dshacp"
	"strawboss/internal/harness/opencode"
	"strawboss/internal/registry"
)

// NewHarness builds the WorkerHarness a model config names. LoadModels
// validates Harness, so unknown values here mean a config/runner skew.
func NewHarness(mc config.ModelConfig, dir, stateDir string) (harness.WorkerHarness, error) {
	switch mc.Harness {
	case "opencode", "":
		return opencode.New(mc, dir, filepath.Join(stateDir, "logs")), nil
	case "dsh":
		return dshacp.New(dir, stateDir), nil
	default:
		return nil, fmt.Errorf("model %q: unknown harness %q", mc.Name, mc.Harness)
	}
}

// aborter is the optional harness capability the timeout path uses.
type aborter interface {
	Abort(ctx context.Context, workerID string) error
}

// pider exposes a worker's subprocess pid for the registry (dsh workers
// are killed from the TUI by pid; opencode workers by session abort).
type pider interface {
	PID(workerID string) int
}

// Outcome is one worker's run: either Err is set (the worker never ran or
// couldn't be tracked) or Res carries the terse result.
type Outcome struct {
	WorkerID string
	Res      harness.Result
	Duration time.Duration
	Err      error
}

// Run runs one task to completion. Cancelling ctx aborts the worker and
// records it failed rather than leaving it running unobserved. warn (may be
// nil) receives non-fatal problems — an abort that failed, a registry write
// that failed — for the caller's log.
func Run(ctx context.Context, h harness.WorkerHarness, reg *registry.Registry, mc config.ModelConfig, task, dir string, warn func(string)) (oc Outcome) {
	if warn == nil {
		warn = func(string) {}
	}
	start := time.Now()
	session, err := h.Spawn(ctx, task, mc)
	if err != nil {
		oc.Err = err
		return oc
	}
	pid := 0
	if p, ok := h.(pider); ok {
		pid = p.PID(session)
	}
	oc.WorkerID, err = reg.Allocate(session, mc.Name, task, dir, pid)
	if err != nil {
		oc.Err = err
		return oc
	}

	res, err := h.Result(ctx, session)
	if err != nil {
		if ctx.Err() == nil {
			oc.Err = err
			return oc
		}
		// Timed out or interrupted: abort the worker, report failed.
		if a, ok := h.(aborter); ok {
			abortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if aerr := a.Abort(abortCtx, session); aerr != nil {
				warn(aerr.Error())
			}
		}
		res = harness.Result{
			WorkerID: session,
			Status:   harness.StatusFailed,
			Summary:  fmt.Sprintf("aborted: %v after %s", context.Cause(ctx), time.Since(start).Round(time.Second)),
		}
	}
	oc.Res = res
	oc.Duration = time.Since(start)

	var usage harness.Usage
	if u, uerr := h.Usage(context.Background(), session); uerr == nil {
		usage = u
	}
	if err := reg.Finish(oc.WorkerID, session, string(res.Status), res.Summary, res.LogPath,
		oc.Duration, usage.InputTokens, usage.OutputTokens); err != nil {
		warn(err.Error())
	}
	return oc
}
