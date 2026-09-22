package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func skipNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// gitRun runs a git command in dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

// newWorkerBranch creates a worker worktree via Create, writes file with
// content into it, and commits it. Returns the worktree path and branch.
func newWorkerBranch(t *testing.T, repo, root, name, file, content string) (path, branch string) {
	t.Helper()
	var err error
	path, branch, err = Create(repo, root, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	committed, err := CommitAll(path, "worker: wrote "+file)
	if err != nil || !committed {
		t.Fatalf("commit: committed=%v err=%v", committed, err)
	}
	return path, branch
}

func TestListBranches(t *testing.T) {
	skipNoGit(t)
	repo := initRepo(t)

	// No worker branches: empty slice, no error.
	branches, err := ListBranches(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 0 {
		t.Fatalf("no branches: got %d", len(branches))
	}

	content := "l1\nl2\nl3\n"
	path, branch := newWorkerBranch(t, repo, t.TempDir(), "w-list", "out.txt", content)

	branches, err = ListBranches(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 1 {
		t.Fatalf("branches = %d, want 1: %+v", len(branches), branches)
	}
	b := branches[0]
	if b.Name != branch {
		t.Errorf("Name = %q, want %q", b.Name, branch)
	}
	if b.Worktree != path {
		t.Errorf("Worktree = %q, want %q", b.Worktree, path)
	}
	if b.Ahead != 1 {
		t.Errorf("Ahead = %d, want 1", b.Ahead)
	}
	if b.Files != 1 {
		t.Errorf("Files = %d, want 1", b.Files)
	}
	if b.Added != 3 {
		t.Errorf("Added = %d, want 3", b.Added)
	}
	if b.Deleted != 0 {
		t.Errorf("Deleted = %d, want 0", b.Deleted)
	}
	if b.Subject != "worker: wrote out.txt" {
		t.Errorf("Subject = %q", b.Subject)
	}
	if b.When.IsZero() {
		t.Errorf("When is zero")
	}
}

func TestDiff(t *testing.T) {
	skipNoGit(t)
	repo := initRepo(t)
	content := "alpha\nbeta\n"
	_, branch := newWorkerBranch(t, repo, t.TempDir(), "w-diff", "out.txt", content)

	out, err := Diff(repo, branch, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"out.txt", "+alpha", "+beta"} {
		if !strings.Contains(out, want) {
			t.Errorf("diff missing %q:\n%s", want, out)
		}
	}

	// A tiny cap truncates with the marker line.
	cut, err := Diff(repo, branch, 32)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cut, "diff truncated") {
		t.Errorf("truncated diff missing marker:\n%s", cut)
	}
	if !strings.Contains(cut, "bytes)") {
		t.Errorf("truncated marker missing byte count:\n%s", cut)
	}
}

func TestMerge(t *testing.T) {
	skipNoGit(t)
	repo := initRepo(t)
	content := "merged\n"
	path, branch := newWorkerBranch(t, repo, t.TempDir(), "w-merge", "out.txt", content)

	if err := Merge(repo, branch); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(repo, "out.txt"))
	if err != nil || string(got) != content {
		t.Errorf("merged file: %v %q", err, got)
	}
	branches, err := ListBranches(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 0 {
		t.Errorf("branch still listed after merge: %+v", branches)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("worktree dir still exists after merge: %v", err)
	}
}

func TestMergeRefusesDirtyRepo(t *testing.T) {
	skipNoGit(t)
	repo := initRepo(t)
	_, branch := newWorkerBranch(t, repo, t.TempDir(), "w-dirty", "out.txt", "x\n")

	// Dirty a tracked file in the main repo.
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Merge(repo, branch); err == nil {
		t.Fatal("merge accepted a dirty repo")
	}
	branches, err := ListBranches(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 1 || branches[0].Name != branch {
		t.Errorf("branch not intact after refused merge: %+v", branches)
	}
}

func TestMergeConflict(t *testing.T) {
	skipNoGit(t)
	repo := initRepo(t)
	// Worker branch edits the tracked file one way…
	_, branch := newWorkerBranch(t, repo, t.TempDir(), "w-conf", "base.txt", "worker edit\n")
	// …and main edits it another way.
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("main edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "main edit")

	err := Merge(repo, branch)
	if err == nil {
		t.Fatal("merge succeeded, want conflict error")
	}
	if !strings.Contains(err.Error(), "base.txt") {
		t.Errorf("conflict error does not name the file: %v", err)
	}
	status := gitRun(t, repo, "status", "--porcelain")
	if status != "" {
		t.Errorf("repo not clean after conflicted merge: %q", status)
	}
	branches, err := ListBranches(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 1 || branches[0].Name != branch {
		t.Errorf("conflicted branch lost: %+v", branches)
	}
}

func TestDiscard(t *testing.T) {
	skipNoGit(t)
	repo := initRepo(t)
	path, branch := newWorkerBranch(t, repo, t.TempDir(), "w-disc", "out.txt", "x\n")

	if err := Discard(repo, branch); err != nil {
		t.Fatal(err)
	}
	branches, err := ListBranches(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 0 {
		t.Errorf("branch still listed after discard: %+v", branches)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("worktree dir still exists after discard: %v", err)
	}
	// Main repo untouched.
	if _, err := os.Stat(filepath.Join(repo, "out.txt")); !os.IsNotExist(err) {
		t.Errorf("discarded branch leaked into main repo: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "base.txt")); err != nil || string(got) != "base\n" {
		t.Errorf("main repo base.txt changed: %v %q", err, got)
	}
}

func TestForeignBranchesRefused(t *testing.T) {
	skipNoGit(t)
	repo := initRepo(t)

	if _, err := Diff(repo, "main", 0); err == nil {
		t.Error("Diff accepted non-strawboss branch")
	}
	if err := Merge(repo, "main"); err == nil {
		t.Error("Merge accepted non-strawboss branch")
	}
	if err := Discard(repo, "main"); err == nil {
		t.Error("Discard accepted non-strawboss branch")
	}
	// main must still be intact (gitRun fails the test on error).
	gitRun(t, repo, "rev-parse", "--verify", "main")
}
