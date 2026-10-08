package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/deadline"
	"github.com/joshgriffith1124/strawboss/internal/harness"
	"github.com/joshgriffith1124/strawboss/internal/registry"
	"github.com/joshgriffith1124/strawboss/internal/repomap"
	"github.com/joshgriffith1124/strawboss/internal/router"
	"github.com/joshgriffith1124/strawboss/internal/runner"
)

// Roles and pipelines (roles.toml) are opt-in: a task gets a role only
// through --role or --pipeline, and without roles.toml delegation behaves
// exactly as before.

// stickyAt returns the value of the sticky flag governing task i, with
// pinFor's rules (tasks written before the first flag bind to it too), ""
// when the flag never appears. "none" clears an earlier value for the
// tasks after it, so one call can mix role and plain tasks.
func stickyAt(pins []modelPin, i int) string {
	if len(pins) == 0 {
		return ""
	}
	v := pins[0].name
	for _, p := range pins {
		if p.from <= i {
			v = p.name
		}
	}
	if v == "none" {
		return ""
	}
	return v
}

// roleWant is the model a role's task is routed toward when the task does
// not name one with --model: the role's model, else the first model in its
// tier, else auto routing. Routing then resolves it against availability
// and free slots like any pinned task.
func roleWant(rc config.RoleConfig, models []config.ModelConfig) string {
	if rc.Model != "" {
		return rc.Model
	}
	if rc.Tier != 0 {
		if mc, ok := config.TierModel(models, rc.Tier); ok {
			return mc.Name
		}
	}
	return router.Auto
}

// rolePrompt frames a task for a role: who the worker is, the role's
// preamble, and — for read-only roles — the rule the runner enforces.
func rolePrompt(rc config.RoleConfig, task string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are working in the %q role.", rc.Name)
	if rc.Prompt != "" {
		b.WriteString("\n" + rc.Prompt)
	}
	if rc.ReadOnly {
		b.WriteString("\n\nThis role is READ-ONLY: do not create, modify or delete any file " +
			"(edits are detected and reverted, and the step fails). Investigate and report — your reply is your whole deliverable.")
	}
	b.WriteString("\n\nTask:\n" + task)
	return b.String()
}

// Pipelines hand each step's full reply to the next step; one step's
// report is cut to this many bytes so a rambling plan cannot crowd a
// small model's context.
const maxHandoffBytes = 12000

// verdictInstruction is appended to a pipeline's reviewing step when the
// pipeline allows fix rounds: the delegate process reads the last line.
const verdictInstruction = "\n\nEnd your reply with exactly one line: `VERDICT: PASS` when the work needs no changes, " +
	"or `VERDICT: CHANGES` when it does — with the required changes listed above that line, most important first."

// parseVerdict finds the reviewer's VERDICT line, scanning from the end:
// "PASS", "CHANGES", or "" when there is none.
func parseVerdict(report string) string {
	lines := strings.Split(report, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.ToUpper(strings.Trim(strings.TrimSpace(lines[i]), "*_`# "))
		rest, ok := strings.CutPrefix(l, "VERDICT:")
		if !ok {
			continue
		}
		rest = strings.Trim(strings.TrimSpace(rest), "*_`. ")
		switch {
		case strings.HasPrefix(rest, "PASS"):
			return "PASS"
		case strings.HasPrefix(rest, "CHANGES"):
			return "CHANGES"
		}
		return ""
	}
	return ""
}

// stepResult is one finished pipeline step, kept for the steps after it.
type stepResult struct {
	role, worker, report string
}

// stepPrompt builds one pipeline step's task: where it sits in the chain,
// the overall task, the earlier steps' reports, and any extra instruction
// (review findings for a fix round, the verdict rule for the reviewer).
func stepPrompt(p config.PipelineConfig, step int, task string, history []stepResult, extra string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This is step %d of %d in the %q pipeline (%s).\n\nOverall task:\n%s",
		step+1, len(p.Steps), p.Name, strings.Join(p.Steps, " → "), task)
	if len(history) > 0 {
		b.WriteString("\n\nWork so far, from the earlier steps:")
		for _, h := range history {
			r := h.report
			if len(r) > maxHandoffBytes {
				r = r[:maxHandoffBytes] + "\n…(cut)"
			}
			if strings.TrimSpace(r) == "" {
				r = "(no report)"
			}
			fmt.Fprintf(&b, "\n\n### %s (%s)\n%s", h.role, h.worker, r)
		}
	}
	b.WriteString(extra)
	return b.String()
}

// pipelineEnv is what every step of one pipeline task shares.
type pipelineEnv struct {
	roles    config.Roles
	models   []config.ModelConfig
	stateDir string
	dir      string // the task's working directory
	repoMap  string
	reg      registry.Registry // copied per step with the step's role
	timeout  time.Duration     // the --timeout flag (0 = per model)
	model    string            // an explicit --model for the task, "" = per role
	route    func(want, task string) (config.ModelConfig, error)
	decorate func(*harness.Result) // the verify gate, for writing steps
	warn     func(string)
	// step replaces runStep in tests; nil runs real workers.
	step func(ctx context.Context, rc config.RoleConfig, prompt, task, pipeline, label string) runner.Outcome
}

// runStep runs one role step to completion on its routed model. pipeline
// and step ("2/3", "fix 1 · security") are stamped on the worker's
// registry events so its tile says where in the chain it is.
func (e pipelineEnv) runStep(ctx context.Context, rc config.RoleConfig, prompt, task, pipeline, step string) runner.Outcome {
	want := e.model
	if want == "" {
		want = roleWant(rc, e.models)
	}
	mc, err := e.route(want, task)
	if err != nil {
		return runner.Outcome{Err: err}
	}
	h, err := runner.NewHarness(mc, e.dir, e.stateDir)
	if err != nil {
		return runner.Outcome{Err: err}
	}
	stepCtx, cancel := deadline.WithTimeout(ctx, timeoutFor(e.timeout, mc))
	defer cancel()
	var decorate func(*harness.Result)
	if rc.ReadOnly {
		stepCtx = runner.WithReadOnly(stepCtx)
	} else {
		decorate = e.decorate
	}
	reg := e.reg
	reg.Role, reg.Pipeline, reg.Step = rc.Name, pipeline, step
	return runner.Run(stepCtx, h, &reg, mc, repomap.Prompt(e.repoMap, rolePrompt(rc, prompt)), task, e.dir, e.warn, decorate)
}

// runPipeline runs one task through a pipeline's steps in order, in the
// task's own working directory — steps are sequential, so they never
// race, and nothing waits on a branch merge. Each step's full report is
// handed to the next.
//
// With fix_rounds, every read-only step that follows a writing step is a
// GATE (a reviewer, security, QA in report mode): it ends on a VERDICT
// line, and CHANGES sends its findings back to the nearest writing step
// before it, then the gate re-checks — up to fix_rounds times per gate,
// so one gate's findings never spend another's budget. A gate still
// requesting changes stops the chain failed. Read-only steps before any
// writer (an architect) only plan. The result is one terse outcome for
// the whole chain.
func runPipeline(ctx context.Context, e pipelineEnv, p config.PipelineConfig, task string) runner.Outcome {
	start := time.Now()
	var (
		history []stepResult
		trail   []string // "architect w12 ✓" per step run
		added   []string
		changed []string
		last    runner.Outcome
		total   int      // fix rounds across all gates
		gates   []string // per gate: "security PASS"
	)
	// writerBefore is the nearest writing step before step i, -1 if none.
	writerBefore := func(i int) int {
		for j := i - 1; j >= 0; j-- {
			if rc, _ := e.roles.Role(p.Steps[j]); !rc.ReadOnly {
				return j
			}
		}
		return -1
	}
	finish := func(oc runner.Outcome, status harness.Status, note string) runner.Outcome {
		oc.Duration = time.Since(start)
		oc.Added, oc.Changed = added, changed
		head := "pipeline " + p.Name + ": " + strings.Join(trail, " → ")
		if note != "" {
			head += " · " + note
		}
		oc.Res.Status = status
		oc.Res.Summary = head + "\n" + oc.Res.Summary
		return oc
	}
	run := func(step int, extra, label string) (runner.Outcome, bool) {
		rc, _ := e.roles.Role(p.Steps[step])
		exec := e.runStep
		if e.step != nil {
			exec = e.step
		}
		oc := exec(ctx, rc, stepPrompt(p, step, task, history, extra), task, p.Name, label)
		if oc.Err != nil {
			trail = append(trail, rc.Name+" ✗")
			return oc, false
		}
		added = unionAdded(added, oc.Added)
		changed = unionAdded(changed, oc.Changed)
		mark := "✓"
		if oc.Res.Status != harness.StatusDone {
			mark = "✗"
		}
		trail = append(trail, rc.Name+" "+oc.WorkerID+" "+mark)
		history = append(history, stepResult{role: rc.Name, worker: oc.WorkerID, report: oc.Res.Report})
		return oc, oc.Res.Status == harness.StatusDone
	}

	n := len(p.Steps)
	summary := func() string {
		if p.FixRounds == 0 {
			return ""
		}
		note := fmt.Sprintf("%d fix round(s)", total)
		if len(gates) == 1 {
			// One gate: "verdict PASS", the shape a single reviewer reads as.
			return note + " · verdict " + gates[0][strings.IndexByte(gates[0], ' ')+1:]
		}
		return note + " · " + strings.Join(gates, " · ")
	}
	for i := range p.Steps {
		gate := p.FixRounds > 0 && writerBefore(i) >= 0
		if rc, _ := e.roles.Role(p.Steps[i]); !rc.ReadOnly {
			gate = false
		}
		extra := ""
		if gate {
			extra = verdictInstruction
		}
		label := fmt.Sprintf("%d/%d", i+1, n)
		oc, ok := run(i, extra, label)
		last = oc
		if oc.Err != nil {
			return oc
		}
		if !ok {
			return finish(oc, harness.StatusFailed, "stopped at "+p.Steps[i])
		}
		if !gate {
			continue
		}

		name := p.Steps[i]
		verdict := parseVerdict(oc.Res.Report)
		for rounds := 1; verdict == "CHANGES" && rounds <= p.FixRounds; rounds++ {
			total++
			findings := last.Res.Report
			if len(findings) > maxHandoffBytes {
				findings = findings[:maxHandoffBytes] + "\n…(cut)"
			}
			w := writerBefore(i)
			fix, ok := run(w, fmt.Sprintf("\n\nFix round %d of %d: the %s step requested changes. Address these findings in the working tree, then stop:\n%s",
				rounds, p.FixRounds, name, findings), fmt.Sprintf("fix %d · %s", rounds, name))
			if fix.Err != nil {
				return fix
			}
			if !ok {
				return finish(fix, harness.StatusFailed, fmt.Sprintf("fix round %d for %s failed", rounds, name))
			}
			oc, ok := run(i, verdictInstruction, fmt.Sprintf("%s recheck %d", label, rounds))
			last = oc
			if oc.Err != nil {
				return oc
			}
			if !ok {
				return finish(oc, harness.StatusFailed, fmt.Sprintf("%s recheck %d failed", name, rounds))
			}
			verdict = parseVerdict(oc.Res.Report)
		}
		switch verdict {
		case "CHANGES":
			gates = append(gates, name+" CHANGES")
			return finish(last, harness.StatusFailed, fmt.Sprintf("%d fix round(s) · %s still requests changes", total, name))
		case "PASS":
			gates = append(gates, name+" PASS")
		default:
			gates = append(gates, name+" no verdict line")
		}
	}
	return finish(last, harness.StatusDone, summary())
}
