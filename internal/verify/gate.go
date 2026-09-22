package verify

import (
	"context"
	"fmt"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/harness"
)

// Gate is the per-worker verify check for one task: delegate and the TUI's
// retry path both run workers through it, so a "done" means the same thing
// whichever of them started the worker.
//
// NewGate takes the tree's fingerprint BEFORE the task's first attempt;
// Decorate (a runner.Run decorate hook) skips the check when the attempt
// left the tree unchanged — a read-only task shouldn't pay for a test
// suite — and otherwise runs it, appends Note's line to the summary, and
// turns a failed check into a failed worker. The baseline stays that of
// the first attempt, so an escalated attempt is still checked when the
// first left changes behind.
type Gate struct {
	ctx      context.Context
	dir      string
	spec     Spec
	baseline string
	haveBase bool
	lastFail string
}

// NewGate prepares the gate for a task in dir. A spec with no Command
// yields a gate that never checks. Outside a git tree there is no baseline
// and the check always runs.
func NewGate(ctx context.Context, dir string, spec Spec) *Gate {
	g := &Gate{ctx: ctx, dir: dir, spec: spec}
	if spec.Command != "" {
		g.baseline, g.haveBase = Fingerprint(dir)
	}
	return g
}

// Decorate is the runner.Run decorate hook. Only a worker that reported
// done is checked: a failure is already a failure.
func (g *Gate) Decorate(res *harness.Result) {
	if g.spec.Command == "" || res.Status != harness.StatusDone || g.unchanged() {
		return
	}
	r := Run(g.ctx, g.dir, g.spec)
	res.Summary += "\n" + Note(g.spec, r)
	if r.OK {
		return
	}
	res.Status = harness.StatusFailed
	g.lastFail = Excerpt(r.Output, 12, 1500)
	if g.lastFail == "" { // a silent failure is still a failure to pass on
		g.lastFail = fmt.Sprintf("(the check printed nothing; exit code %d)", r.ExitCode)
	}
}

// LastFailure is the failing tail of the most recent failed check ("" if
// none) — what an escalated attempt is told instead of rediscovering it.
func (g *Gate) LastFailure() string { return g.lastFail }

// Command is the check's command line ("" when there is no check).
func (g *Gate) Command() string { return g.spec.Command }

func (g *Gate) unchanged() bool {
	if !g.haveBase {
		return false
	}
	now, ok := Fingerprint(g.dir)
	return ok && now == g.baseline
}

// Note is the one bracketed line a reader (supervisor, TUI row) gets about
// a check, plus the failing tail when it failed. The excerpt is small on
// purpose: the terse-result budget is ~250 tokens per worker.
func Note(spec Spec, r Result) string {
	d := r.Duration.Round(time.Second)
	switch {
	case r.OK:
		return fmt.Sprintf("[verified: `%s` passed in %s]", spec.Command, d)
	case r.TimedOut:
		return fmt.Sprintf("[verify TIMED OUT after %s: `%s` — counted as failed]", d, spec.Command)
	default:
		note := fmt.Sprintf("[verify FAILED: `%s` exit %d after %s]", spec.Command, r.ExitCode, d)
		if ex := Excerpt(r.Output, 6, 400); ex != "" {
			note += "\n" + ex
		}
		return note
	}
}
