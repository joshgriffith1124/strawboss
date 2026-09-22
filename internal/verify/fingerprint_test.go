package verify

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitOrSkip skips the test when git is not on PATH.
func gitOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// runGit runs git with cmd.Dir = dir and fails the test on a non-zero exit.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newRepo creates a git repo in a fresh temp dir with the identity
// configured locally so commits work without a global config.
func newRepo(t *testing.T) string {
	t.Helper()
	gitOrSkip(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "verify@example.com")
	runGit(t, dir, "config", "user.name", "Verify Test")
	return dir
}

// commitAll stages every file in dir and commits with msg.
func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "-c", "commit.gpgsign=false", "commit", "-q", "-m", msg)
}

// mustFingerprint is Fingerprint, failing the test when ok is false.
func mustFingerprint(t *testing.T, dir string) string {
	t.Helper()
	fp, ok := Fingerprint(dir)
	if !ok {
		t.Fatalf("Fingerprint(%s): ok=false, fp=%q", dir, fp)
	}
	return fp
}

func mustWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFingerprintNotInRepo(t *testing.T) {
	gitOrSkip(t)
	dir := t.TempDir()
	// t.TempDir could itself sit inside a work tree (e.g. a checkout used
	// as TMPDIR); that would make the test meaningless, so skip it.
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) == "true" {
		t.Skipf("temp dir %s is inside a git work tree", dir)
	}
	mustWrite(t, dir, "a.txt", "x")
	if fp, ok := Fingerprint(dir); ok {
		t.Fatalf("Fingerprint: ok=true, fp=%q; want ok=false outside a work tree", fp)
	}
}

func TestFingerprintStable(t *testing.T) {
	dir := newRepo(t)
	mustWrite(t, dir, "a.txt", "hello\n")
	commitAll(t, dir, "initial")
	fp0 := mustFingerprint(t, dir)
	fp1 := mustFingerprint(t, dir)
	if fp0 != fp1 {
		t.Fatalf("unchanged tree: %q != %q", fp0, fp1)
	}
}

func TestFingerprintTrackedChange(t *testing.T) {
	dir := newRepo(t)
	mustWrite(t, dir, "a.txt", "hello\n")
	commitAll(t, dir, "initial")
	fp0 := mustFingerprint(t, dir)

	mustWrite(t, dir, "a.txt", "hello world\n")
	if fp := mustFingerprint(t, dir); fp == fp0 {
		t.Fatal("modified tracked file did not change the fingerprint")
	}

	// Reverting the content exactly returns to the original state.
	mustWrite(t, dir, "a.txt", "hello\n")
	if fp := mustFingerprint(t, dir); fp != fp0 {
		t.Fatalf("reverted tree: %q != original %q", fp, fp0)
	}
}

func TestFingerprintUntrackedAdd(t *testing.T) {
	dir := newRepo(t)
	mustWrite(t, dir, "a.txt", "hello\n")
	commitAll(t, dir, "initial")
	fp0 := mustFingerprint(t, dir)

	mustWrite(t, dir, "b.txt", "new\n")
	fp1 := mustFingerprint(t, dir)
	if fp1 == fp0 {
		t.Fatal("new untracked file did not change the fingerprint")
	}

	// Editing the untracked file's content changes it again.
	mustWrite(t, dir, "b.txt", "newer content\n")
	if fp := mustFingerprint(t, dir); fp == fp1 {
		t.Fatal("untracked content change did not change the fingerprint")
	}
}

func TestFingerprintNewCommit(t *testing.T) {
	dir := newRepo(t)
	mustWrite(t, dir, "a.txt", "hello\n")
	commitAll(t, dir, "initial")
	fp0 := mustFingerprint(t, dir)

	mustWrite(t, dir, "a.txt", "changed\n")
	commitAll(t, dir, "second")
	if fp := mustFingerprint(t, dir); fp == fp0 {
		t.Fatal("new commit did not change the fingerprint")
	}
}

func TestFingerprintIgnoredFile(t *testing.T) {
	dir := newRepo(t)
	mustWrite(t, dir, "a.txt", "hello\n")
	mustWrite(t, dir, ".gitignore", "ignored.txt\n")
	commitAll(t, dir, "initial")
	fp0 := mustFingerprint(t, dir)

	mustWrite(t, dir, "ignored.txt", "should not count\n")
	if fp := mustFingerprint(t, dir); fp != fp0 {
		t.Fatal("adding an ignored file changed the fingerprint")
	}
}

func TestFingerprintNoCommits(t *testing.T) {
	dir := newRepo(t) // init + local identity only, no commit
	if _, ok := Fingerprint(dir); !ok {
		t.Fatal("Fingerprint: ok=false in a repo with no commits yet")
	}
	fp0 := mustFingerprint(t, dir)
	mustWrite(t, dir, "a.txt", "hello\n")
	if fp := mustFingerprint(t, dir); fp == fp0 {
		t.Fatal("untracked file in a no-commit repo did not change the fingerprint")
	}
}
