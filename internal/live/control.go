package live

import (
	"context"
	"path/filepath"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/registry"
	"github.com/joshgriffith1124/strawboss/internal/repomap"
	"github.com/joshgriffith1124/strawboss/internal/runner"
	"github.com/joshgriffith1124/strawboss/internal/ui"
	"github.com/joshgriffith1124/strawboss/internal/verify"
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
	pid := o.workerPID[id]
	started := o.workerStarted[id]
	unfinished := o.unfinished[id]
	o.mu.Unlock()

	if session == "" || !unfinished {
		o.emitAsync(ui.ToastMsg{Text: id + " isn't running — nothing to kill"})
		return
	}
	// A worker that died without reporting (power loss, crash) has no one
	// left to record its end, and after a reboot its pid may belong to an
	// unrelated process: never signal it — close it out as failed.
	isDsh := false
	if mc, ok := o.modelConfig(model); ok && mc.Harness == "dsh" {
		isDsh = true
	}
	if deadBeforeBoot(started) || (isDsh && !workerAlive(pid, started)) {
		if o.reapOne(id, session) {
			o.emitAsync(ui.ToastMsg{Text: id + " was already dead — marked failed (process lost)"})
		}
		return
	}
	// dsh workers are subprocesses owned by their delegate: SIGTERM the
	// recorded pid; the waiting delegate then records the failure. No pid
	// on record (pre-upgrade registry rows) is a displayed state.
	if isDsh {
		if pid <= 0 {
			o.emitAsync(ui.ToastMsg{Text: "kill " + id + ": no pid on record"})
			return
		}
		if err := killWorker(pid); err != nil {
			o.emitAsync(ui.ToastMsg{Text: "kill " + id + ": " + err.Error()})
			return
		}
		o.emitAsync(ui.ToastMsg{Text: id + " killed — waiting for the result to close"})
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
	mc, found := o.modelConfig(model)
	if !found {
		o.emitAsync(ui.ToastMsg{Text: "retry " + id + ": no model config " + model})
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	h, err := runner.NewHarness(mc, dir, o.StateDir)
	if err != nil {
		o.emitAsync(ui.ToastMsg{Text: "retry " + id + ": " + err.Error()})
		return
	}
	// The project's verify check applies to retries exactly as to
	// delegations: without it a retried worker's "done" would be taken on
	// faith. A broken .strawboss.toml refuses, as delegate does.
	check, err := verify.Find(dir)
	if err != nil {
		o.emitAsync(ui.ToastMsg{Text: "retry " + id + ": " + err.Error()})
		return
	}
	o.mu.Lock()
	run := o.RunID
	o.mu.Unlock()
	o.emitAsync(ui.ToastMsg{Text: "retrying " + id + "'s task on " + model})
	go func() {
		ctx, cancel := context.WithTimeout(ctx, retryTimeout)
		defer cancel()
		reg := &registry.Registry{Path: filepath.Join(o.StateDir, "workers.jsonl"), Run: run}
		warn := func(s string) { o.emitAsync(ui.RawLogMsg{Source: "wrk", Line: s}) }
		prompt := repomap.Prompt(repomap.Build(dir, 0), task)
		gate := verify.NewGate(ctx, dir, check)
		if oc := runner.Run(ctx, h, reg, mc, prompt, task, dir, warn, gate.Decorate); oc.Err != nil {
			o.emitAsync(ui.ToastMsg{Text: "retry of " + id + " failed: " + oc.Err.Error()})
		}
	}()
}
