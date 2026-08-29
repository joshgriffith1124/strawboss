// Package worktree isolates workers in git worktrees: each worker gets
// its own checkout on its own strawboss/* branch, so parallel workers
// cannot clobber the shared working directory. Work is committed on the
// worker's branch only — merging into the user's branch stays a human
// decision; strawboss never commits to a branch it does not own.
package worktree

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// BranchPrefix namespaces worker branches.
const BranchPrefix = "strawboss/"

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// IsRepo reports whether dir is inside a git work tree.
func IsRepo(dir string) bool {
	out, err := git(dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// Create adds a worktree at <root>/<name> on a new branch
// strawboss/<name>, branched from repoDir's current HEAD.
func Create(repoDir, root, name string) (path, branch string, err error) {
	path = filepath.Join(root, name)
	branch = BranchPrefix + name
	if _, err := git(repoDir, "worktree", "add", "-b", branch, path); err != nil {
		return "", "", fmt.Errorf("creating worktree: %w", err)
	}
	return path, branch, nil
}

// CommitAll stages and commits everything in the worktree. Returns false
// with no error when there is nothing to commit.
func CommitAll(path, message string) (bool, error) {
	if _, err := git(path, "add", "-A"); err != nil {
		return false, fmt.Errorf("committing worktree: %w", err)
	}
	if out, err := git(path, "status", "--porcelain"); err != nil {
		return false, fmt.Errorf("committing worktree: %w", err)
	} else if out == "" {
		return false, nil
	}
	// A worktree may run on a box with no git identity configured; the
	// commit is on a strawboss-owned branch, so a fixed identity is fine.
	if _, err := git(path, "-c", "user.name=strawboss", "-c", "user.email=strawboss@localhost",
		"commit", "-q", "-m", message); err != nil {
		return false, fmt.Errorf("committing worktree: %w", err)
	}
	return true, nil
}

// Remove deletes a worktree and its branch — used when a worker produced
// no changes, so nothing of value is lost.
func Remove(repoDir, path, branch string) error {
	if _, err := git(repoDir, "worktree", "remove", "--force", path); err != nil {
		return fmt.Errorf("removing worktree: %w", err)
	}
	if _, err := git(repoDir, "branch", "-D", branch); err != nil {
		return fmt.Errorf("removing worktree branch: %w", err)
	}
	return nil
}
