package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"strawboss/internal/config"
	"strawboss/internal/worktree"
)

// runClean is the retention sweep: worker logs and dsh session logs older
// than --age go, and worktrees whose strawboss/* branch is fully merged
// are removed. Unmerged worktrees are NEVER deleted, only listed — clean
// must not be able to destroy work.
func runClean(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	age := fs.Duration("age", 14*24*time.Hour, "delete logs and sessions older than this")
	dryRun := fs.Bool("dry-run", false, "report what would be removed without removing it")
	stateDir := fs.String("state-dir", "", "state directory (default ~/.strawboss)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *stateDir == "" {
		var err error
		if *stateDir, err = config.DefaultStateDir(); err != nil {
			return err
		}
	}
	cutoff := time.Now().Add(-*age)
	removed, kept := 0, 0

	act := func(kind, path string, remove func() error) {
		if *dryRun {
			fmt.Fprintf(stdout, "would remove %s %s\n", kind, path)
			removed++
			return
		}
		if err := remove(); err != nil {
			fmt.Fprintf(stdout, "keeping %s %s: %v\n", kind, path, err)
			kept++
			return
		}
		removed++
	}

	// Worker transcript logs: one file per worker.
	logs, _ := filepath.Glob(filepath.Join(*stateDir, "logs", "*.jsonl"))
	for _, path := range logs {
		if fi, err := os.Stat(path); err == nil && fi.ModTime().Before(cutoff) {
			act("log", path, func() error { return os.Remove(path) })
		}
	}

	// dsh session logs: <root>/<project>/<session>/ directories.
	sessions, _ := filepath.Glob(filepath.Join(*stateDir, "dsh-sessions", "*", "*"))
	for _, path := range sessions {
		fi, err := os.Stat(path)
		if err != nil || !fi.IsDir() || !fi.ModTime().Before(cutoff) {
			continue
		}
		act("session", path, func() error { return os.RemoveAll(path) })
	}

	// Worktrees: removable only when the branch is fully merged into the
	// repo's HEAD; anything else is listed for the human.
	wts, _ := filepath.Glob(filepath.Join(*stateDir, "worktrees", "*"))
	for _, path := range wts {
		repo, branch, err := worktreeOrigin(path)
		if err != nil {
			fmt.Fprintf(stdout, "skipping worktree %s: %v\n", path, err)
			kept++
			continue
		}
		if !branchMerged(repo, branch) {
			fmt.Fprintf(stdout, "keeping worktree %s: branch %s not merged (review or delete it yourself)\n", path, branch)
			kept++
			continue
		}
		act("worktree", path, func() error { return worktree.Remove(repo, path, branch) })
	}

	verb := "removed"
	if *dryRun {
		verb = "would remove"
	}
	fmt.Fprintf(stdout, "%s %d item(s), kept %d\n", verb, removed, kept)
	return nil
}

// worktreeOrigin resolves a worktree's main repository and checked-out
// branch from git itself — never guessed from paths.
func worktreeOrigin(path string) (repo, branch string, err error) {
	out, err := gitOut(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", "", err
	}
	repo = filepath.Dir(out) // <repo>/.git → <repo>
	branch, err = gitOut(path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", "", err
	}
	if !strings.HasPrefix(branch, worktree.BranchPrefix) {
		return "", "", fmt.Errorf("branch %q is not strawboss-owned", branch)
	}
	return repo, branch, nil
}

func branchMerged(repo, branch string) bool {
	out, err := gitOut(repo, "branch", "--merged", "HEAD", "--list", branch)
	return err == nil && strings.TrimSpace(out) != ""
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
