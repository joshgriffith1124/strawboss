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

	"strawboss/internal/config"
	"strawboss/internal/harness"
	"strawboss/internal/registry"
	"strawboss/internal/runner"
	"strawboss/internal/worktree"
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
	useWorktree := fs.Bool("worktree", false, "run each worker in an isolated git worktree on its own strawboss/* branch")
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

	warn := func(s string) { fmt.Fprintf(os.Stderr, "strawboss: %s\n", s) }
	outcomes := make([]runner.Outcome, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
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
			// Per-worker harness: with worktrees every worker has its own
			// working directory, which the harness carries.
			h, err := runner.NewHarness(mc, workerDir, *stateDir)
			if err != nil {
				outcomes[i] = runner.Outcome{Err: err}
				return
			}
			outcomes[i] = runner.Run(ctx, h, reg, mc, task, workerDir, warn, decorate)
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
