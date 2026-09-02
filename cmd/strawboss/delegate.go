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
	"github.com/joshgriffith1124/strawboss/internal/runner"
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
	model := fs.String("model", "", "model config name from models.toml (required)")
	var tasks []string
	registerTaskFlags(fs, &tasks)
	dir := fs.String("dir", "", "worker working directory (default: current directory)")
	useWorktree := fs.Bool("worktree", false, "run each worker in an isolated git worktree on its own strawboss/* branch")
	escalate := fs.Bool("escalate", true, "on worker failure, retry the task once on the next model config in models.toml (skipped with --worktree)")
	useRepoMap := fs.Bool("repomap", true, "prepend a compact repo map (files + top-level symbols) to worker prompts")
	timeout := fs.Duration("timeout", 20*time.Minute, "abort workers after this long")
	stateDir := fs.String("state-dir", "", "state directory (default ~/.strawboss)")
	modelsPath := fs.String("models", "", "models.toml path (default <state-dir>/models.toml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *model == "" || len(tasks) == 0 {
		return errors.New("delegate: --model and at least one --task or --task-file are required")
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

	// Delegation-loop guard: a task identical to one that already failed
	// twice this run gets refused, not retried — re-delegating the same
	// task into the same wall is the classic supervisor loop.
	refused := failedTaskCounts(filepath.Join(*stateDir, "workers.jsonl"), os.Getenv("STRAWBOSS_RUN"), *model)

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

	outcomes := make([]runner.Outcome, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
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
		var decorate func(*harness.Result)
		if wts != nil {
			workerDir = wts[i].path
			wt := wts[i]
			decorate = func(res *harness.Result) { finalizeWorktree(res, *dir, wt.path, wt.branch, task) }
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
			// model re-dispatches ONCE to the next config in models.toml,
			// inside this call — the supervisor still reads a single terse
			// result instead of hand-retrying. Worktree runs are excluded:
			// the second attempt would share the first's partial state and
			// worktree finalization runs per attempt.
			if *escalate && wts == nil && ctx.Err() == nil && !succeeded(outcomes[i]) {
				if next, ok := nextModel(models, mc.Name); ok {
					warn("escalating failed task to " + next.Name)
					outcomes[i] = runEscalated(ctx, outcomes[i], next, mc.Name,
						prompt, task, workerDir, *stateDir, reg, warn, decorate)
				}
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
	if failed > 0 {
		return fmt.Errorf("%d of %d workers failed", failed, len(tasks))
	}
	return nil
}

// succeeded reports whether an outcome is a completed, non-failed worker.
func succeeded(oc runner.Outcome) bool {
	return oc.Err == nil && oc.Res.Status == harness.StatusDone
}

// nextModel returns the config after cur in models.toml file order — the
// escalation ladder IS the preference order: entries below the requested
// one are the fallbacks.
func nextModel(models []config.ModelConfig, cur string) (config.ModelConfig, bool) {
	for i, m := range models {
		if m.Name == cur && i+1 < len(models) {
			return models[i+1], true
		}
	}
	return config.ModelConfig{}, false
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
