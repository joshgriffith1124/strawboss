package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo makes a git repo with one commit.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "t"},
		{"config", "user.email", "t@t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func TestIsRepo(t *testing.T) {
	if !IsRepo(initRepo(t)) {
		t.Error("repo not recognized")
	}
	if IsRepo(t.TempDir()) {
		t.Error("bare tempdir recognized as repo")
	}
}

func TestCreateCommitRemove(t *testing.T) {
	repo := initRepo(t)
	root := t.TempDir()

	path, branch, err := Create(repo, root, "w-test-1")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "strawboss/w-test-1" {
		t.Errorf("branch = %q", branch)
	}
	// The worktree sees the base commit.
	if _, err := os.Stat(filepath.Join(path, "base.txt")); err != nil {
		t.Fatalf("worktree missing base file: %v", err)
	}

	// Nothing to commit yet.
	committed, err := CommitAll(path, "empty")
	if err != nil || committed {
		t.Fatalf("empty commit: committed=%v err=%v", committed, err)
	}

	// Worker output commits on the branch, untouched in the main repo.
	if err := os.WriteFile(filepath.Join(path, "out.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	committed, err = CommitAll(path, "strawboss w1: did the thing")
	if err != nil || !committed {
		t.Fatalf("commit: committed=%v err=%v", committed, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "out.txt")); !os.IsNotExist(err) {
		t.Error("worker output leaked into the main checkout")
	}
	show := exec.Command("git", "show", "--stat", branch)
	show.Dir = repo
	out, err := show.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "out.txt") {
		t.Errorf("branch commit: %v %s", err, out)
	}

	// A second worktree removes cleanly when unused.
	p2, b2, err := Create(repo, root, "w-test-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := Remove(repo, p2, b2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p2); !os.IsNotExist(err) {
		t.Error("worktree dir still exists after Remove")
	}
}
