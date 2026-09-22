package snapshot

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func tgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func tRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	tgit(t, dir, "init", "-q")
	tgit(t, dir, "config", "user.name", "t")
	tgit(t, dir, "config", "user.email", "t@t")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, dir, "add", "-A")
	tgit(t, dir, "commit", "-qm", "init")
	return dir
}

func TestTakeTouchesNothing(t *testing.T) {
	dir := tRepo(t)
	for name, content := range map[string]string{"a.txt": "x\n", "c.txt": "c\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st := tgit(t, dir, "status", "--porcelain")
	stash := tgit(t, dir, "stash", "list")
	h, err := Take(dir)
	if err != nil || h == "" {
		t.Fatalf("Take = %q, %v; want non-empty hash", h, err)
	}
	if got := tgit(t, dir, "status", "--porcelain"); got != st {
		t.Fatalf("status changed: before %q, after %q", st, got)
	}
	if got := tgit(t, dir, "stash", "list"); got != stash {
		t.Fatalf("stash list changed: before %q, after %q", stash, got)
	}
}

func TestTakeEmptyRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	tgit(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Take(dir); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreFromSubdir(t *testing.T) {
	dir := tRepo(t)
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "s.txt"), []byte("s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, dir, "add", "-A")
	tgit(t, dir, "commit", "-qm", "sub")

	before, err := Take(sub)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := Take(sub)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(sub, "w1", "before", before); err != nil {
		t.Fatal(err)
	}
	if err := Save(sub, "w1", "after", after); err != nil {
		t.Fatal(err)
	}
	files, err := Restore(sub, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "a.txt" {
		t.Fatalf("files = %v, want [a.txt]", files)
	}
	b, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "a\n" {
		t.Fatalf("a.txt = %q, want %q", b, "a\n")
	}
}

func TestWanted(t *testing.T) {
	dir := tRepo(t)
	if !Wanted(dir) {
		t.Error("Wanted(repo) = false, want true")
	}
	if Wanted(t.TempDir()) {
		t.Error("Wanted(empty dir) = true, want false")
	}
	wt := filepath.Join(t.TempDir(), "wt")
	tgit(t, dir, "worktree", "add", "-q", wt, "-b", "wt")
	if Wanted(wt) {
		t.Error("Wanted(linked worktree) = true, want false")
	}
}
