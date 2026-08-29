package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"strawboss/internal/worktree"
)

func TestClean(t *testing.T) {
	stateDir := t.TempDir()
	old := time.Now().Add(-30 * 24 * time.Hour)

	// One old and one fresh worker log.
	logDir := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldLog := filepath.Join(logDir, "old.jsonl")
	freshLog := filepath.Join(logDir, "fresh.jsonl")
	for _, p := range []string{oldLog, freshLog} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(oldLog, old, old); err != nil {
		t.Fatal(err)
	}

	// One old dsh session dir.
	sess := filepath.Join(stateDir, "dsh-sessions", "proj", "ses_old")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sess, old, old); err != nil {
		t.Fatal(err)
	}

	// A repo with a merged and an unmerged strawboss worktree.
	repo := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(repo, "init", "-q", "-b", "main")
	run(repo, "config", "user.name", "t")
	run(repo, "config", "user.email", "t@t")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "-A")
	run(repo, "commit", "-q", "-m", "base")

	wtRoot := filepath.Join(stateDir, "worktrees")
	if err := os.MkdirAll(wtRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// Merged: a worktree branch with no extra commits is merged by definition.
	if _, _, err := worktree.Create(repo, wtRoot, "merged-1"); err != nil {
		t.Fatal(err)
	}
	// Unmerged: a commit on the branch that main doesn't have.
	unmergedPath, _, err := worktree.Create(repo, wtRoot, "unmerged-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unmergedPath, "work.txt"), []byte("w\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.CommitAll(unmergedPath, "unmerged work"); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := runClean([]string{"--state-dir", stateDir}, &out); err != nil {
		t.Fatalf("clean: %v\n%s", err, out.String())
	}
	got := out.String()

	if _, err := os.Stat(oldLog); !os.IsNotExist(err) {
		t.Error("old log survived")
	}
	if _, err := os.Stat(freshLog); err != nil {
		t.Error("fresh log removed")
	}
	if _, err := os.Stat(sess); !os.IsNotExist(err) {
		t.Error("old session survived")
	}
	if _, err := os.Stat(filepath.Join(wtRoot, "merged-1")); !os.IsNotExist(err) {
		t.Error("merged worktree survived")
	}
	if _, err := os.Stat(unmergedPath); err != nil {
		t.Error("unmerged worktree was DELETED — clean destroyed work")
	}
	if !strings.Contains(got, "not merged") {
		t.Errorf("output missing unmerged notice:\n%s", got)
	}

	// Dry run removes nothing.
	out.Reset()
	if err := runClean([]string{"--state-dir", stateDir, "--dry-run"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(freshLog); err != nil {
		t.Error("dry run touched files")
	}
}
