package live

import (
	"fmt"
	"path/filepath"
	"syscall"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/registry"
	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// Reaping closes out workers whose process died without writing its
// finished event (power loss, crash, reboot). Left alone they sit "running"
// forever, and a recorded pid may since have been recycled to an UNRELATED
// process — so nothing may signal that pid blindly (docs/ROADMAP.md item 27).

// reapSummary is the terse reason stamped on a reaped worker's finished event.
const reapSummary = "process lost (worker died without reporting; e.g. power loss or crash)"

// bootTime and pidExists are package-level vars so tests can substitute
// fakes (reap_test.go) instead of probing the real machine. The platform
// boot-time source lives in reap_linux.go / reap_other.go.
var (
	bootTime  = defaultBootTime
	pidExists = func(pid int) bool {
		// Signal 0 is a liveness probe: EPERM means the process exists but
		// is not signalable by us (still alive); ESRCH means it is gone.
		err := syscall.Kill(pid, 0)
		return err == nil || err == syscall.EPERM
	}
)

// killWorker signals a live dsh worker's subprocess; a var so tests can
// count signals without spawning a real process.
var killWorker = func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

// workerAlive reports whether a pid-backed worker could still be running.
// An unknown pid (<= 0) is never evidence of death; a pid that predates the
// last boot has been recycled and may belong to anyone, so it is dead even
// if the pid happens to exist now.
func workerAlive(pid int, started time.Time) bool {
	if pid <= 0 {
		return true
	}
	if deadBeforeBoot(started) {
		return false
	}
	return pidExists(pid)
}

// reapDead marks every worker this run considers unfinished but which can be
// proven dead as failed ("process lost"). It runs once at orchestrator
// startup, before the registry watcher begins: the finished events it writes
// are picked up by the watcher and flow into the UI through the normal finish
// path (so no second, hand-rolled upsert is needed here). o.started is
// re-stamped afterwards so those events count as replayed history — the
// deaths happened in the past, and must not ring the bell or push. It is
// idempotent — a reaped worker already carries its finished event.
func (o *Orchestrator) reapDead() {
	defer func() { o.started = time.Now() }()
	reg := &registry.Registry{Path: filepath.Join(o.StateDir, "workers.jsonl"), Run: o.RunID}
	events, err := reg.Load()
	if err != nil {
		o.emitAsync(ui.ToastMsg{Text: "reap: " + err.Error()})
		return
	}
	type state struct {
		session  string
		pid      int
		started  time.Time
		finished bool
	}
	st := map[string]state{}
	var order []string
	for _, ev := range events {
		if o.RunID != "" && ev.Run != "" && ev.Run != o.RunID {
			continue // another run's worker — not this session's story
		}
		s := st[ev.Worker]
		if _, seen := st[ev.Worker]; !seen {
			order = append(order, ev.Worker)
		}
		switch ev.Type {
		case "spawned":
			s.session, s.pid, s.started = ev.Session, ev.PID, ev.TS
		case "finished":
			s.finished = true
		}
		st[ev.Worker] = s
	}
	for _, wid := range order {
		s := st[wid]
		if s.finished || s.started.IsZero() {
			continue // already closed, or never spawned (still queued)
		}
		var dead bool
		if s.pid > 0 {
			// pid-backed (dsh): dead when the process is gone or the pid
			// predates the last boot.
			dead = !workerAlive(s.pid, s.started)
		} else {
			// no pid on record (opencode, or a pre-upgrade row): the boot
			// clock is the only evidence of death.
			dead = deadBeforeBoot(s.started)
		}
		if !dead {
			continue
		}
		if err := reg.Finish(wid, s.session, "failed", reapSummary, "", 0, 0, 0, 0); err != nil {
			o.emitAsync(ui.ToastMsg{Text: fmt.Sprintf("reap %s: %s", wid, err.Error())})
		}
	}
}

// reapOne closes out one dead worker on demand (the TUI kill path). The
// finished event reaches the UI through the registry watcher like any other;
// o.reaped marks it so that event replays silent — the death happened in
// the past, and the caller toasts instead of the bell/push announcing a
// fresh failure. Reports whether the event was written.
func (o *Orchestrator) reapOne(wid, session string) bool {
	reg := &registry.Registry{Path: filepath.Join(o.StateDir, "workers.jsonl"), Run: o.RunID}
	o.mu.Lock()
	o.reaped[wid] = true
	o.mu.Unlock()
	if err := reg.Finish(wid, session, "failed", reapSummary, "", 0, 0, 0, 0); err != nil {
		o.emitAsync(ui.ToastMsg{Text: fmt.Sprintf("reap %s: %s", wid, err.Error())})
		return false
	}
	return true
}

// deadBeforeBoot reports whether a worker started before the machine last
// booted — the one death proof that needs no pid (opencode workers).
func deadBeforeBoot(started time.Time) bool {
	boot := bootTime()
	return !boot.IsZero() && !started.IsZero() && started.Before(boot)
}
