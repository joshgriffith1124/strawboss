package live

import (
	"context"
	"path/filepath"
	"time"

	"strawboss/internal/config"
	"strawboss/internal/harness/opencode"
	"strawboss/internal/registry"
	"strawboss/internal/runner"
	"strawboss/internal/ui"
)

// retryTimeout bounds a TUI-spawned retry worker, matching delegate's
// default --timeout.
const retryTimeout = 20 * time.Minute

// OnKillWorker aborts a running worker's opencode session (ui hook,
// dashboard `x`). The process waiting on the worker — the supervisor's
// delegate invocation, or a retry runner — sees the session go idle with an
// incomplete reply and records the worker failed through the normal result
// path, so the row closes and the supervisor (if involved) gets a terse
// failure like any other.
func (o *Orchestrator) OnKillWorker(id string) {
	o.mu.Lock()
	session := o.workerSession[id]
	model := o.workerModel[id]
	unfinished := o.unfinished[id]
	o.mu.Unlock()

	if session == "" || !unfinished {
		o.emitAsync(ui.ToastMsg{Text: id + " isn't running — nothing to kill"})
		return
	}
	c := o.clientFor(model)
	if c == nil {
		o.emitAsync(ui.ToastMsg{Text: "kill " + id + ": no model config " + model})
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.Abort(ctx, session); err != nil {
			o.emitAsync(ui.ToastMsg{Text: "kill " + id + ": " + err.Error()})
			return
		}
		o.emitAsync(ui.ToastMsg{Text: id + " killed — waiting for the result to close"})
	}()
}

// OnRetryWorker re-runs a finished worker's task as a NEW worker with the
// same model and directory (ui hook, dashboard `r`). The TUI spawns it
// itself through the harness + registry — zero supervisor tokens (the
// supervisor is not told; this is manual operator control). The registry
// watcher flows the new row into the table like any delegation.
func (o *Orchestrator) OnRetryWorker(id string) {
	o.mu.Lock()
	task := o.workerTask[id]
	model := o.workerModel[id]
	dir := o.workerDir[id]
	unfinished := o.unfinished[id]
	ctx := o.runCtx
	o.mu.Unlock()

	if unfinished {
		o.emitAsync(ui.ToastMsg{Text: id + " is still running — kill it first"})
		return
	}
	if task == "" {
		o.emitAsync(ui.ToastMsg{Text: "retry " + id + ": no task on record"})
		return
	}
	var mc config.ModelConfig
	found := false
	for _, m := range o.Models {
		if m.Name == model {
			mc, found = m, true
			break
		}
	}
	if !found {
		o.emitAsync(ui.ToastMsg{Text: "retry " + id + ": no model config " + model})
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	o.emitAsync(ui.ToastMsg{Text: "retrying " + id + "'s task on " + model})
	go func() {
		ctx, cancel := context.WithTimeout(ctx, retryTimeout)
		defer cancel()
		h := opencode.New(mc, dir, filepath.Join(o.StateDir, "logs"))
		reg := &registry.Registry{Path: filepath.Join(o.StateDir, "workers.jsonl"), Run: o.RunID}
		warn := func(s string) { o.emitAsync(ui.RawLogMsg{Source: "wrk", Line: s}) }
		if oc := runner.Run(ctx, h, reg, mc, task, dir, warn); oc.Err != nil {
			o.emitAsync(ui.ToastMsg{Text: "retry of " + id + " failed: " + oc.Err.Error()})
		}
	}()
}
