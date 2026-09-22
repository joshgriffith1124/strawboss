package snapshot

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func rgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func rRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	rgit(t, dir, "init", "-q")
	rgit(t, dir, "config", "user.name", "t")
	rgit(t, dir, "config", "user.email", "t@t")
	rWrite(t, filepath.Join(dir, "a.txt"), "a\n")
	rWrite(t, filepath.Join(dir, "b.txt"), "b\n")
	rgit(t, dir, "add", "-A")
	rgit(t, dir, "commit", "-qm", "init")
	return dir
}

func rWrite(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRestoreUndoesWorker(t *testing.T) {
	dir := rRepo(t)
	before, _ := Take(dir)
	rWrite(t, filepath.Join(dir, "a.txt"), "x\n")
	os.Remove(filepath.Join(dir, "b.txt"))
	rWrite(t, filepath.Join(dir, "c.txt"), "c\n")
	after, _ := Take(dir)
	if err := Save(dir, "w1", "before", before); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, "w1", "after", after); err != nil {
		t.Fatal(err)
	}
	files, err := Restore(dir, "w1")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	if s := strings.Join(files, ","); s != "a.txt,b.txt,c.txt" {
		t.Fatalf("files = %v, want [a.txt b.txt c.txt]", files)
	}
	if got := rRead(t, filepath.Join(dir, "a.txt")); got != "a\n" {
		t.Fatalf("a.txt = %q, want %q", got, "a\n")
	}
	if got := rRead(t, filepath.Join(dir, "b.txt")); got != "b\n" {
		t.Fatalf("b.txt = %q, want %q", got, "b\n")
	}
	if _, err := os.Stat(filepath.Join(dir, "c.txt")); !os.IsNotExist(err) {
		t.Fatal("c.txt still exists")
	}
	if _, _, ok := Refs(dir, "w1"); ok {
		t.Fatal("refs not dropped")
	}
}

func TestRestoreConflict(t *testing.T) {
	dir := rRepo(t)
	before, _ := Take(dir)
	rWrite(t, filepath.Join(dir, "a.txt"), "x\n")
	rWrite(t, filepath.Join(dir, "c.txt"), "c\n")
	after, _ := Take(dir)
	if err := Save(dir, "w1", "before", before); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, "w1", "after", after); err != nil {
		t.Fatal(err)
	}
	rWrite(t, filepath.Join(dir, "a.txt"), "later\n")
	if _, err := Restore(dir, "w1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "c.txt")); err != nil {
		t.Fatalf("c.txt touched despite conflict: %v", err)
	}
}

func TestRestoreLeavesOtherLaterEdits(t *testing.T) {
	dir := rRepo(t)
	before, _ := Take(dir)
	rWrite(t, filepath.Join(dir, "a.txt"), "x\n")
	after, _ := Take(dir)
	if err := Save(dir, "w1", "before", before); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, "w1", "after", after); err != nil {
		t.Fatal(err)
	}
	rWrite(t, filepath.Join(dir, "b.txt"), "later\n")
	if _, err := Restore(dir, "w1"); err != nil {
		t.Fatal(err)
	}
	if got := rRead(t, filepath.Join(dir, "a.txt")); got != "a\n" {
		t.Fatalf("a.txt = %q, want %q", got, "a\n")
	}
	if got := rRead(t, filepath.Join(dir, "b.txt")); got != "later\n" {
		t.Fatalf("b.txt = %q, want %q", got, "later\n")
	}
}

func TestRestoreNoChanges(t *testing.T) {
	dir := rRepo(t)
	before, _ := Take(dir)
	after, _ := Take(dir)
	if err := Save(dir, "w1", "before", before); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, "w1", "after", after); err != nil {
		t.Fatal(err)
	}
	if files, err := Restore(dir, "w1"); err != nil {
		t.Fatal(err)
	} else if files != nil {
		t.Fatalf("files = %v, want nil", files)
	}
}
