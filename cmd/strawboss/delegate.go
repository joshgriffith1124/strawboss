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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/harness"
	"github.com/joshgriffith1124/strawboss/internal/live"
	"github.com/joshgriffith1124/strawboss/internal/registry"
	"github.com/joshgriffith1124/strawboss/internal/repomap"
	"github.com/joshgriffith1124/strawboss/internal/router"
	"github.com/joshgriffith1124/strawboss/internal/runner"
	"github.com/joshgriffith1124/strawboss/internal/verify"
	"github.com/joshgriffith1124/strawboss/internal/worktree"
)

// runDelegate is the command the supervisor calls to spawn workers. Its
// stdout is the ONLY thing that enters supervisor context (invariant 3):
// per worker, one header line and a few-line summary — target ≤ ~250
// tokens each. Everything else (live transcripts, registry events) flows
// to the TUI locally.
//
// --task (or --task-file) may repeat: each task becomes its own worker and
// they all run in parallel inside this one invocation (a single Bash call
// on the supervisor side — compound shell commands don't pass the
// allowlist).
// registerTaskFlags wires --task and --task-file onto fs, appending to
// tasks in the order the flags appear so the two forms interleave freely.
//
// --task-file exists to keep task prose out of the shell. A task
// containing $( ), backticks, or < > reads to Claude Code's Bash
// allowlist as a compound command, and the whole delegate call is denied
// however correct the prefix is — three real denials in the transcripts
// (docs/NOTES.md). A file path carries no metacharacters.
func registerTaskFlags(fs *flag.FlagSet, tasks *[]string) {
	fs.Func("task", "task prompt for a worker (repeat for parallel workers)", func(s string) error {
		*tasks = append(*tasks, s)
		return nil
	})
	fs.Func("task-file", "read a task prompt from a file (repeat for parallel workers)", func(path string) error {
		task, err := readTaskFile(path)
		if err != nil {
			return err
		}
		*tasks = append(*tasks, task)
		return nil
	})
}

// readTaskFile loads one task prompt from disk. Surrounding whitespace is
// trimmed (a heredoc or editor leaves a trailing newline); an empty file
// is an error rather than a worker spawned with nothing to do.
func readTaskFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading task file: %w", err)
	}
	task := strings.TrimSpace(string(b))
	if task == "" {
		return "", fmt.Errorf("task file %s is empty", path)
	}
	return task, nil
}

func runDelegate(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("delegate", flag.ContinueOnError)
	var tasks []string
	registerTaskFlags(fs, &tasks)
	// --model is sticky: it binds the tasks that FOLLOW it, so one call can
	// fan out across tiers (--model fast --task a --model big --task b).
	var pins []modelPin
	fs.Func("model", "model config name from models.toml, or \"auto\" (the default) to let strawboss route each task by weight; applies to the tasks that follow it", func(s string) error {
		pins = append(pins, modelPin{name: s, from: len(tasks)})
		return nil
	})
	dir := fs.String("dir", "", "worker working directory (default: current directory)")
	useWorktree := fs.Bool("worktree", false, "run each worker in an isolated git worktree on its own strawboss/* branch")
	escalate := fs.Bool("escalate", true, "on worker failure, retry the task once on the next model config in models.toml — the next tier up when tiers are set (skipped with --worktree)")
	useRepoMap := fs.Bool("repomap", true, "prepend a compact repo map (files + top-level symbols) to worker prompts")
	timeout := fs.Duration("timeout", 20*time.Minute, "abort workers after this long")
	stateDir := fs.String("state-dir", "", "state directory (default ~/.strawboss)")
	modelsPath := fs.String("models", "", "models.toml path (default <state-dir>/models.toml)")
	verifyFlag := fs.String("verify", "", "check command run after each worker, overriding the project's "+verify.FileName+" verify; \"off\" skips the check")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(tasks) == 0 {
		return errors.New("delegate: at least one --task or --task-file is required")
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

	// Budget guard: the TUI writes a stop marker when the run's ceiling is
	// hit; refusing here is a terse tool result the supervisor reads —
	// stopping costs almost nothing in its context.
	if reason, err := os.ReadFile(live.BudgetStopFile(*stateDir, *dir)); err == nil {
		fmt.Fprintf(stdout, "delegation blocked by the budget guard: %s\nDo NOT retry or work around this. Report current status to the user and wait.\n",
			strings.TrimSpace(string(reason)))
		return errors.New("budget ceiling reached")
	}

	models, err := config.LoadModels(*modelsPath)
	if err != nil {
		return err
	}
	check, err := verifySpec(*dir, *verifyFlag)
	if err != nil {
		return fmt.Errorf("delegate: %w", err)
	}
	routes, err := routeTasks(models, tasks, pins, *modelsPath, live.Available)
	if err != nil {
		return err
	}

	// Delegation-loop guard: a task identical to one that already failed
	// twice this run gets refused, not retried — re-delegating the same
	// task into the same wall is the classic supervisor loop. Counted per
	// model the task actually lands on.
	refusedBy := map[string]map[string]int{}
	for _, r := range routes {
		if _, ok := refusedBy[r.mc.Name]; !ok {
			refusedBy[r.mc.Name] = failedTaskCounts(filepath.Join(*stateDir, "workers.jsonl"), os.Getenv("STRAWBOSS_RUN"), r.mc.Name)
		}
	}

	warn := func(s string) { fmt.Fprintf(os.Stderr, "strawboss: %s\n", s) }

	// Worktree isolation: every task gets its own checkout + branch
	// BEFORE anything spawns, so a failure here aborts the whole call.
	type wtInfo struct{ path, branch string }
	var wts []wtInfo
	if *useWorktree {
		if !worktree.IsRepo(*dir) {
			return fmt.Errorf("delegate: --worktree: %s is not a git repository", *dir)
		}
		root := filepath.Join(*stateDir, "worktrees")
		if err := os.MkdirAll(root, 0o755); err != nil {
			return fmt.Errorf("delegate: %w", err)
		}
		now := time.Now()
		stamp := fmt.Sprintf("%s-%s", now.Format("20060102-150405"),
			strconv.FormatInt(now.UnixNano()%1e9, 36))
		wts = make([]wtInfo, len(tasks))
		for i := range tasks {
			path, branch, err := worktree.Create(*dir, root, fmt.Sprintf("%s-t%d", stamp, i+1))
			if err != nil {
				return fmt.Errorf("delegate: %w", err)
			}
			wts[i] = wtInfo{path, branch}
			// Gitignored-but-needed files (.env, local certs) named in
			// .worktreeinclude; without them a task "works in the main
			// checkout, fails in the worktree". Non-fatal: the worker may
			// not need them.
			if _, err := worktree.CopyIncludes(*dir, path); err != nil {
				warn(err.Error())
			}
		}
	}

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

	// One map for the whole call: worktrees are branches of the same
	// HEAD, so the main checkout's map serves every worker.
	repoMap := ""
	if *useRepoMap {
		repoMap = repomap.Build(*dir, 0)
	}

	// Verify gate. Per worker when each worker has a directory to itself
	// (a worktree, or the only task in the call): the check then judges
	// exactly that worker's changes, and a failure escalates like any
	// other. Parallel workers sharing one directory would each be judged
	// on the others' half-finished edits, so there the check runs once,
	// after all of them.
	perWorker := check.Command != "" && (wts != nil || len(tasks) == 1)
	batchCheck := check.Command != "" && !perWorker
	batchBase, haveBatchBase := "", false
	if batchCheck {
		batchBase, haveBatchBase = verify.Fingerprint(*dir)
	}

	outcomes := make([]runner.Outcome, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
		mc := routes[i].mc
		refused := refusedBy[mc.Name]
		if refused[task] >= 2 {
			outcomes[i] = runner.Outcome{Res: harness.Result{
				Status: harness.StatusFailed,
				Summary: "refused: this exact task has already failed " + strconv.Itoa(refused[task]) +
					" times this run. Do NOT resubmit it — change the approach, split it, or ask the user.",
			}}
			if wts != nil {
				if rmErr := worktree.Remove(*dir, wts[i].path, wts[i].branch); rmErr != nil {
					warn(rmErr.Error())
				}
			}
			continue
		}
		workerDir := *dir
		var finalize func(*harness.Result)
		if wts != nil {
			workerDir = wts[i].path
			wt := wts[i]
			finalize = func(res *harness.Result) { finalizeWorktree(res, *dir, wt.path, wt.branch, task) }
		}
		// verifyFail holds the excerpt of this task's last failed check, so
		// an escalated attempt is told what broke instead of rediscovering
		// it. Only this task's goroutine touches it.
		verifyFail := ""
		// Baseline for "did this task change anything": a worker that only
		// read or reported leaves the tree as it found it, and running the
		// project's check after it is minutes of pure waste. Taken before
		// the first attempt, so an escalated attempt is still checked when
		// the first one left changes behind. No baseline (not a git tree)
		// means always check.
		baseline, haveBaseline := "", false
		if perWorker {
			baseline, haveBaseline = verify.Fingerprint(workerDir)
		}
		unchanged := func() bool {
			if !haveBaseline {
				return false
			}
			now, ok := verify.Fingerprint(workerDir)
			return ok && now == baseline
		}
		decorate := func(res *harness.Result) {
			if perWorker && res.Status == harness.StatusDone && !unchanged() {
				r := verify.Run(ctx, workerDir, check)
				res.Summary += "\n" + verifyNote(check, r)
				if !r.OK {
					res.Status = harness.StatusFailed
					verifyFail = verify.Excerpt(r.Output, 12, 1500)
					if verifyFail == "" { // a silent failure is still a failure to pass on
						verifyFail = fmt.Sprintf("(the check printed nothing; exit code %d)", r.ExitCode)
					}
				}
			}
			if finalize != nil {
				finalize(res) // commit after the check: the branch note must be last
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			prompt := repomap.Prompt(repoMap, task)
			// Per-worker harness: with worktrees every worker has its own
			// working directory, which the harness carries.
			h, err := runner.NewHarness(mc, workerDir, *stateDir)
			if err != nil {
				outcomes[i] = runner.Outcome{Err: err}
				return
			}
			outcomes[i] = runner.Run(ctx, h, reg, mc, prompt, task, workerDir, warn, decorate)
			// Cheap-first escalation: a failure on the requested (cheap)
			// model re-dispatches ONCE to the next config in models.toml
			// (the next tier up when tiers are set), inside this call — the
			// supervisor still reads a single terse result instead of
			// hand-retrying. Availability is probed fresh: minutes have
			// passed since dispatch, and escalating into a dead model burns
			// a guaranteed second failure. Worktree runs are excluded: the
			// second attempt would share the first's partial state and
			// worktree finalization runs per attempt.
			if *escalate && wts == nil && ctx.Err() == nil && !succeeded(outcomes[i]) {
				up := func(m config.ModelConfig) bool { ok, _ := live.Available(ctx, m); return ok }
				if next, ok := router.Next(models, mc.Name, up); ok {
					warn("escalating failed task to " + next.Name)
					retryPrompt := prompt
					if verifyFail != "" {
						retryPrompt += "\n\nA previous attempt at this task left the project failing its check (`" +
							check.Command + "`). Its changes are still in the working tree — fix them or redo the task so the check passes:\n" +
							verifyFail
					}
					outcomes[i] = runEscalated(ctx, outcomes[i], next, mc.Name,
						retryPrompt, task, workerDir, *stateDir, reg, warn, decorate)
				}
			}
			// The routing note rides on the supervisor-facing summary only:
			// the registry and TUI already show which model each worker ran on.
			if note := routes[i].note; note != "" && outcomes[i].Err == nil {
				outcomes[i].Res.Summary = note + "\n" + outcomes[i].Res.Summary
			}
			if outcomes[i].Err != nil && wts != nil {
				// The worker never ran or couldn't be tracked: nothing of
				// value in the worktree; drop it.
				if rmErr := worktree.Remove(*dir, wts[i].path, wts[i].branch); rmErr != nil {
					warn(rmErr.Error())
				}
			}
		}()
	}
	wg.Wait()

	failed := 0
	for i, oc := range outcomes {
		if oc.Err != nil {
			failed++
			fmt.Fprintf(stdout, "task %d error: %v\n", i+1, oc.Err)
			continue
		}
		// The terse result. Keep this format stable: the TUI pairs the
		// supervisor's tool_result against it, and the supervisor reads it.
		fmt.Fprintf(stdout, "%s %s %s · log %s\n%s\n",
			oc.WorkerID, oc.Res.Status, oc.Duration.Round(time.Second), oc.Res.LogPath, oc.Res.Summary)
		if oc.Res.Status != harness.StatusDone {
			failed++
		}
	}
	// Shared-directory batch: one check over everyone's combined changes.
	// It cannot say which worker broke it, so worker statuses stand and the
	// call as a whole fails.
	batchFailed := false
	if haveBatchBase {
		if now, ok := verify.Fingerprint(*dir); ok && now == batchBase {
			batchCheck = false // nobody changed anything: nothing to check
		}
	}
	if batchCheck && failed < len(tasks) {
		vctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		r := verify.Run(vctx, *dir, check)
		cancel()
		fmt.Fprintf(stdout, "%s (after all %d workers — it cannot tell which one broke it)\n",
			verifyNote(check, r), len(tasks))
		batchFailed = !r.OK
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d workers failed", failed, len(tasks))
	}
	if batchFailed {
		return errors.New("verify check failed after the workers finished")
	}
	return nil
}

// verifySpec resolves the check for a call: --verify wins ("off" disables),
// else the project's .strawboss.toml. A broken settings file is an error —
// silently skipping the check would pass off unverified work as verified.
func verifySpec(dir, flagValue string) (verify.Spec, error) {
	switch flagValue {
	case "off":
		return verify.Spec{}, nil
	case "":
		return verify.Find(dir)
	}
	spec, err := verify.Find(dir)
	if err != nil {
		spec = verify.Spec{} // the flag replaces the file; its timeout is lost with it
	}
	spec.Command, spec.Source = flagValue, "--verify"
	return spec, nil
}

// verifyNote is the one bracketed line the supervisor reads about a check,
// plus the failing tail when it failed. The excerpt is small on purpose:
// the terse-result budget is ~250 tokens per worker.
func verifyNote(spec verify.Spec, r verify.Result) string {
	d := r.Duration.Round(time.Second)
	switch {
	case r.OK:
		return fmt.Sprintf("[verified: `%s` passed in %s]", spec.Command, d)
	case r.TimedOut:
		return fmt.Sprintf("[verify TIMED OUT after %s: `%s` — counted as failed]", d, spec.Command)
	default:
		note := fmt.Sprintf("[verify FAILED: `%s` exit %d after %s]", spec.Command, r.ExitCode, d)
		if ex := verify.Excerpt(r.Output, 6, 400); ex != "" {
			note += "\n" + ex
		}
		return note
	}
}

// succeeded reports whether an outcome is a completed, non-failed worker.
func succeeded(oc runner.Outcome) bool {
	return oc.Err == nil && oc.Res.Status == harness.StatusDone
}

// modelPin is one --model flag: name binds the tasks from index from on,
// until the next pin.
type modelPin struct {
	name string
	from int
}

// pinFor returns the --model value governing task i. Tasks written before
// the first --model bind to it too (the supervisor sometimes puts --task
// first); no --model at all means auto.
func pinFor(pins []modelPin, i int) string {
	if len(pins) == 0 {
		return router.Auto
	}
	name := pins[0].name
	for _, p := range pins {
		if p.from <= i {
			name = p.name
		}
	}
	return name
}

// route is where one task runs, plus the one-line note that tells the
// supervisor when that was not simply the model it named.
type route struct {
	mc   config.ModelConfig
	note string
}

// routeTasks decides each task's model before anything spawns. A pinned
// task wants the model it names; an auto task wants whatever tier its
// weight maps to (router.Pick). Either way the want is then resolved
// against availability, so a task aimed at a model that is down or not
// loaded runs on the nearest live one instead of failing on arrival. Each
// model is probed at most once per call. When nothing is available the
// task keeps its want and surfaces the real error.
func routeTasks(models []config.ModelConfig, tasks []string, pins []modelPin, modelsPath string,
	available func(context.Context, config.ModelConfig) (bool, string)) ([]route, error) {
	type probe struct {
		ok  bool
		why string
	}
	probes := map[string]probe{}
	check := func(mc config.ModelConfig) probe {
		p, seen := probes[mc.Name]
		if !seen {
			p.ok, p.why = available(context.Background(), mc)
			probes[mc.Name] = p
		}
		return p
	}

	routes := make([]route, len(tasks))
	for i, task := range tasks {
		name := pinFor(pins, i)
		var want config.ModelConfig
		note := ""
		if name == router.Auto {
			var why string
			want, why = router.Pick(models, task)
			if cut := strings.Index(why, " ("); cut >= 0 {
				why = why[:cut] // the signals are for tests and humans, not supervisor tokens
			}
			note = "[auto → " + want.Name + ": " + why + "]"
		} else {
			found := false
			for _, m := range models {
				if m.Name == name {
					want, found = m, true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("delegate: no model %q in %s", name, modelsPath)
			}
		}
		got, ok := router.Resolve(models, want, func(m config.ModelConfig) bool { return check(m).ok })
		if ok && got.Name != want.Name {
			note = "[" + want.Name + " unavailable (" + check(want).why + "); ran on " + got.Name + "]"
		}
		routes[i] = route{mc: got, note: note}
	}
	return routes, nil
}

// runEscalated re-dispatches a failed task once on the next model config.
// The final terse result must tell the whole story in one place: what the
// cheap model did, and how the escalation went. A second attempt that
// never ran keeps the first, richer outcome rather than replacing it.
func runEscalated(ctx context.Context, first runner.Outcome, next config.ModelConfig, firstModel,
	prompt, task, workerDir, stateDir string, reg *registry.Registry,
	warn func(string), decorate func(*harness.Result)) runner.Outcome {
	firstDesc := ""
	if first.Err != nil {
		firstDesc = "error: " + first.Err.Error()
	} else {
		firstDesc = firstLineN(first.Res.Summary, 120)
	}

	h, err := runner.NewHarness(next, workerDir, stateDir)
	if err == nil {
		oc := runner.Run(ctx, h, reg, next, prompt, task, workerDir, warn, decorate)
		if oc.Err == nil {
			oc.Res.Summary = "[escalated from " + firstModel + ", which failed: " + firstDesc + "]\n" + oc.Res.Summary
			return oc
		}
		err = oc.Err
	}
	// Escalation never produced a result: report the first attempt, noting
	// the dead end so the supervisor doesn't re-dispatch to that model.
	if first.Err != nil {
		first.Err = fmt.Errorf("%s failed (%s); escalation to %s errored: %v", firstModel, firstDesc, next.Name, err)
		return first
	}
	first.Res.Summary += "\n[escalation to " + next.Name + " errored: " + err.Error() + "]"
	return first
}

// finalizeWorktree commits whatever the worker left in its worktree onto
// its strawboss/* branch (partial work from failures included) and amends
// the terse summary with where the work lives. A worktree with no changes
// is removed — nothing of value is lost. Merging into the user's branch
// is never done here (the branch is the deliverable).
func finalizeWorktree(res *harness.Result, repoDir, path, branch, task string) {
	msg := "strawboss " + res.WorkerID + ": " + firstLineN(task, 60)
	committed, err := worktree.CommitAll(path, msg)
	switch {
	case err != nil:
		res.Summary += "\n[worktree " + path + ": " + err.Error() + "]"
	case committed:
		res.Summary += "\n[work committed on branch " + branch + " — review/merge it; worktree at " + path + "]"
	default:
		note := "\n[no file changes; worktree removed]"
		if rmErr := worktree.Remove(repoDir, path, branch); rmErr != nil {
			note = "\n[no file changes; " + rmErr.Error() + "]"
		}
		res.Summary += note
	}
}

func firstLineN(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// failedTaskCounts maps task text to how many workers already FAILED it
// in this run for this model — the input to the delegation-loop guard.
func failedTaskCounts(registryPath, run, model string) map[string]int {
	events, err := (&registry.Registry{Path: registryPath}).Load()
	if err != nil {
		return nil
	}
	taskOf := map[string]string{}
	counts := map[string]int{}
	for _, ev := range events {
		if ev.Run != run {
			continue
		}
		switch ev.Type {
		case "spawned":
			if ev.Model == model {
				taskOf[ev.Worker] = ev.Task
			}
		case "finished":
			if ev.Status == "failed" {
				if task, ok := taskOf[ev.Worker]; ok {
					counts[task]++
				}
			}
		}
	}
	return counts
}
