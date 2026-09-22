// Package snapshot records a git working tree before and after a worker runs
// and can undo that worker's edits. It never modifies the working tree,
// index, HEAD or stash except inside Restore.
package snapshot

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func git(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("snapshot: git %s: %w: %s", args[0], err, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// IsRepo reports whether dir is inside a git work tree.
func IsRepo(dir string) bool {
	s, err := git(dir, nil, "rev-parse", "--is-inside-work-tree")
	return err == nil && s == "true"
}

// Wanted reports whether a worker running in dir should be snapshotted: dir
// is in a git work tree and not a linked worktree (a `delegate --worktree`
// checkout, whose work already lands on its own branch).
func Wanted(dir string) bool {
	if !IsRepo(dir) {
		return false
	}
	gd, err1 := git(dir, nil, "rev-parse", "--path-format=absolute", "--git-dir")
	cd, err2 := git(dir, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	return err1 == nil && err2 == nil && gd == cd
}

// Take records the whole working tree at dir (tracked files as they are now
// plus untracked, non-ignored files) as an unreferenced commit and returns
// its hash. It works through a private index, so nothing in the repo moves.
func Take(dir string) (string, error) {
	f, err := os.CreateTemp("", "strawboss-idx-*")
	if err != nil {
		return "", fmt.Errorf("snapshot: temp index: %w", err)
	}
	name := f.Name()
	f.Close()
	os.Remove(name) // git creates it; an empty file is not a valid index
	defer os.Remove(name)
	env := []string{
		"GIT_AUTHOR_NAME=strawboss",
		"GIT_AUTHOR_EMAIL=strawboss@localhost",
		"GIT_COMMITTER_NAME=strawboss",
		"GIT_COMMITTER_EMAIL=strawboss@localhost",
		"GIT_INDEX_FILE=" + name,
	}
	parent, err := git(dir, nil, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		parent = "" // no commits yet
	} else if _, err := git(dir, env, "read-tree", parent); err != nil {
		return "", err
	}
	if _, err := git(dir, env, "add", "-A"); err != nil {
		return "", err
	}
	tree, err := git(dir, env, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	args = append(args, "-m", "strawboss snapshot")
	return git(dir, env, args...)
}

func ref(workerID, which string) string {
	return "refs/strawboss/snap/" + workerID + "/" + which
}

// Save points refs/strawboss/snap/<workerID>/<which> ("before" or
// "after") at a snapshot commit, which also keeps gc from pruning it.
func Save(dir, workerID, which, commit string) error {
	_, err := git(dir, nil, "update-ref", ref(workerID, which), commit)
	return err
}

func Refs(dir, workerID string) (before, after string, ok bool) {
	before, errBefore := git(dir, nil, "rev-parse", "--verify", "-q", ref(workerID, "before"))
	after, errAfter := git(dir, nil, "rev-parse", "--verify", "-q", ref(workerID, "after"))
	return before, after, errBefore == nil && errAfter == nil
}

// Changed lists the repo-relative paths that differ between two snapshots.
func Changed(dir, a, b string) ([]string, error) {
	out, err := git(dir, nil, "diff", "--name-only", "--no-renames", "-z", a, b)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

var ErrConflict = errors.New("files changed since the worker finished")

type ConflictError struct {
	Files []string
}

func (e *ConflictError) Error() string {
	names := e.Files
	suffix := ""
	if len(names) > 5 {
		names = names[:5]
		suffix = ", …"
	}
	return fmt.Sprintf("snapshot: %d file(s) changed since the worker finished: %s%s",
		len(e.Files), strings.Join(names, ", "), suffix)
}

func (e *ConflictError) Unwrap() error { return ErrConflict }

// Drop deletes a worker's snapshot refs; missing refs are fine.
func Drop(dir, workerID string) error {
	for _, which := range []string{"before", "after"} {
		if _, err := git(dir, nil, "rev-parse", "--verify", "-q", ref(workerID, which)); err == nil {
			if _, err := git(dir, nil, "update-ref", "-d", ref(workerID, which)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Restore undoes a worker's edits: every path that changed between its
// before and after snapshots goes back to its before content (or is
// deleted if it did not exist then). If any of those paths changed again
// since the worker finished it returns a *ConflictError and touches
// nothing. Success drops the refs and returns the restored paths
// (repo-relative); a worker that changed nothing returns nil, nil.
func Restore(dir, workerID string) ([]string, error) {
	// Diff paths are relative to the repo root, so work from there.
	top, err := git(dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	dir = top
	before, after, ok := Refs(dir, workerID)
	if !ok {
		return nil, fmt.Errorf("snapshot: no snapshot for worker %s", workerID)
	}
	paths, err := Changed(dir, before, after)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		if err := Drop(dir, workerID); err != nil {
			return nil, err
		}
		return nil, nil
	}
	now, err := Take(dir)
	if err != nil {
		return nil, err
	}
	later, err := Changed(dir, after, now)
	if err != nil {
		return nil, err
	}
	moved := make(map[string]bool, len(later))
	for _, p := range later {
		moved[p] = true
	}
	var conflicts []string
	for _, p := range paths {
		if moved[p] {
			conflicts = append(conflicts, p)
		}
	}
	if len(conflicts) > 0 {
		return nil, &ConflictError{Files: conflicts}
	}
	var keep []string
	for _, p := range paths {
		if _, err := git(dir, nil, "cat-file", "-e", before+":"+p); err == nil {
			keep = append(keep, p)
		} else if err := os.Remove(filepath.Join(dir, p)); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("snapshot: remove %s: %w", p, err)
		}
	}
	if len(keep) > 0 {
		// Literal pathspecs: a file named "*.go" must not glob.
		args := append([]string{"restore", "--source=" + before, "--worktree", "--"}, keep...)
		if _, err := git(dir, []string{"GIT_LITERAL_PATHSPECS=1"}, args...); err != nil {
			return nil, err
		}
	}
	if err := Drop(dir, workerID); err != nil {
		return nil, err
	}
	return paths, nil
}
