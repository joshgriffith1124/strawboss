package repomap

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	add := exec.Command("git", "add", "-A")
	add.Dir = dir
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	return dir
}

func TestBuildSymbolsAndFormat(t *testing.T) {
	dir := initRepo(t, map[string]string{
		"api/server.py": "import os\n\nclass Server:\n    pass\n\ndef run_probe(x):\n    pass\n\nasync def prepare_job():\n    pass\n",
		"pkg/lock.go":   "package pkg\n\ntype SlotLock struct{}\n\nfunc (l *SlotLock) Acquire() {}\n\nfunc NewSlotLock() *SlotLock { return nil }\n",
		"web/app.ts":    "export class App {}\nexport const config = {}\nfunction helper() {}\n",
		"README.md":     "# readme\n",
	})
	m := Build(dir, 0)
	for _, want := range []string{
		"api/server.py: Server, run_probe, prepare_job",
		"pkg/lock.go: SlotLock, Acquire, NewSlotLock",
		"web/app.ts: App, config, helper",
		"README.md",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("map missing %q:\n%s", want, m)
		}
	}
}

func TestBuildBudgetTruncates(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 50; i++ {
		files[filepath.Join("dir", strings.Repeat("f", 20)+string(rune('a'+i%26))+".go")] =
			"package p\n\nfunc Exported() {}\n"
	}
	dir := initRepo(t, files)
	m := Build(dir, 400)
	if len(m) > 500 {
		t.Errorf("map size %d exceeds budget+slack", len(m))
	}
	if !strings.Contains(m, "more files") {
		t.Errorf("truncated map missing the elision note:\n%s", m)
	}
}

func TestBuildNonRepo(t *testing.T) {
	if m := Build(t.TempDir(), 0); m != "" {
		t.Errorf("non-repo map = %q", m)
	}
}

func TestPrompt(t *testing.T) {
	if got := Prompt("", "do it"); got != "do it" {
		t.Errorf("empty map: %q", got)
	}
	got := Prompt("MAP\n", "do it")
	if !strings.HasPrefix(got, "MAP") || !strings.HasSuffix(got, "Your task:\ndo it") {
		t.Errorf("prompt = %q", got)
	}
}
