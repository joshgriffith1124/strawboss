// Package runner executes one worker task to completion: spawn, register,
// wait, record. It is shared by the delegate command (supervisor-initiated)
// and the TUI's manual retry (user-initiated) so both paths register and
// finish workers identically.
package runner

import (
	"context"
	"fmt"
	"time"

	"strawboss/internal/config"
	"strawboss/internal/harness"
	"strawboss/internal/harness/opencode"
	"strawboss/internal/registry"
)

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
func Run(ctx context.Context, h *opencode.Harness, reg *registry.Registry, mc config.ModelConfig, task, dir string, warn func(string)) (oc Outcome) {
	if warn == nil {
		warn = func(string) {}
	}
	start := time.Now()
	session, err := h.Spawn(ctx, task, mc)
	if err != nil {
		oc.Err = err
		return oc
	}
	oc.WorkerID, err = reg.Allocate(session, mc.Name, task, dir)
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
		abortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if aerr := h.Abort(abortCtx, session); aerr != nil {
			warn(aerr.Error())
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
