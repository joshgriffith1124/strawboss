package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFind(t *testing.T) {
	writeFile := func(t *testing.T, dir, body string) string {
		t.Helper()
		path := filepath.Join(dir, FileName)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("file in dir", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "verify = \"go test ./...\"\n")
		spec, err := Find(dir)
		if err != nil {
			t.Fatalf("Find: %v", err)
		}
		want := Spec{Command: "go test ./...", Timeout: defaultTimeout, Source: path}
		if spec != want {
			t.Fatalf("spec = %+v, want %+v", spec, want)
		}
	})

	t.Run("file in a parent while dir is a subdirectory", func(t *testing.T) {
		root := t.TempDir()
		path := writeFile(t, root, "verify = \"make check\"\n")
		sub := filepath.Join(root, "a", "b")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		spec, err := Find(sub)
		if err != nil {
			t.Fatalf("Find: %v", err)
		}
		want := Spec{Command: "make check", Timeout: defaultTimeout, Source: path}
		if spec != want {
			t.Fatalf("spec = %+v, want %+v", spec, want)
		}
	})

	t.Run("stops at a directory containing .git (file above not found)", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "verify = \"echo above\"\n") // must not be found
		proj := filepath.Join(root, "proj")
		if err := os.MkdirAll(filepath.Join(proj, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proj, ".git"), []byte("gitdir: /x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		spec, err := Find(filepath.Join(proj, "sub"))
		if err != nil {
			t.Fatalf("Find: %v", err)
		}
		if spec != (Spec{}) {
			t.Fatalf("spec = %+v, want zero Spec", spec)
		}
	})

	t.Run("stops at a .git directory too", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "verify = \"echo above\"\n")
		proj := filepath.Join(root, "proj")
		if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		spec, err := Find(proj)
		if err != nil {
			t.Fatalf("Find: %v", err)
		}
		if spec != (Spec{}) {
			t.Fatalf("spec = %+v, want zero Spec", spec)
		}
	})

	t.Run("no file anywhere is not an error", func(t *testing.T) {
		dir := t.TempDir()
		spec, err := Find(dir)
		if err != nil {
			t.Fatalf("Find: %v", err)
		}
		if spec != (Spec{}) {
			t.Fatalf("spec = %+v, want zero Spec", spec)
		}
	})

	t.Run("unknown key is an error naming the key", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "verify = \"true\"\nbogus = 1\n")
		if _, err := Find(dir); err == nil || !strings.Contains(err.Error(), "bogus") {
			t.Fatalf("err = %v, want unknown key bogus", err)
		}
	})

	t.Run("invalid verify_timeout is an error", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "verify = \"true\"\nverify_timeout = \"nope\"\n")
		if _, err := Find(dir); err == nil || !strings.Contains(err.Error(), "verify_timeout") {
			t.Fatalf("err = %v, want invalid verify_timeout error", err)
		}
	})

	t.Run("negative verify_timeout is an error", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "verify = \"true\"\nverify_timeout = \"-5s\"\n")
		if _, err := Find(dir); err == nil || !strings.Contains(err.Error(), "negative") {
			t.Fatalf("err = %v, want negative timeout error", err)
		}
	})

	t.Run("custom verify_timeout is honored", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "verify = \"true\"\nverify_timeout = \"10m\"\n")
		spec, err := Find(dir)
		if err != nil {
			t.Fatalf("Find: %v", err)
		}
		want := Spec{Command: "true", Timeout: 10 * time.Minute, Source: path}
		if spec != want {
			t.Fatalf("spec = %+v, want %+v", spec, want)
		}
	})
}

func TestRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	t.Run("exit 0 is OK", func(t *testing.T) {
		res := Run(ctx, dir, Spec{Command: "exit 0"})
		if !res.OK || res.ExitCode != 0 || res.TimedOut {
			t.Fatalf("res = %+v", res)
		}
	})

	t.Run("failure carries exit code and output", func(t *testing.T) {
		res := Run(ctx, dir, Spec{Command: "echo boom; exit 3"})
		if res.OK || res.TimedOut || res.ExitCode != 3 {
			t.Fatalf("res = %+v", res)
		}
		if !strings.Contains(res.Output, "boom") {
			t.Fatalf("output = %q, want it to contain boom", res.Output)
		}
	})

	t.Run("stderr is captured", func(t *testing.T) {
		res := Run(ctx, dir, Spec{Command: "echo err 1>&2; exit 1"})
		if res.OK || res.ExitCode != 1 || !strings.Contains(res.Output, "err") {
			t.Fatalf("res = %+v", res)
		}
	})

	t.Run("timeout kills and returns promptly", func(t *testing.T) {
		start := time.Now()
		res := Run(ctx, dir, Spec{Command: "sleep 5", Timeout: 200 * time.Millisecond})
		elapsed := time.Since(start)
		if !res.TimedOut || res.OK || res.ExitCode != -1 {
			t.Fatalf("res = %+v", res)
		}
		if elapsed >= 5*time.Second {
			t.Fatalf("took %v, want well under 5s", elapsed)
		}
	})

	t.Run("command runs in dir", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "marker"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		res := Run(ctx, dir, Spec{Command: "test -f marker"})
		if !res.OK || res.ExitCode != 0 {
			t.Fatalf("res = %+v", res)
		}
	})

	t.Run("empty command is a vacuous pass", func(t *testing.T) {
		res := Run(ctx, dir, Spec{})
		if !res.OK || res.ExitCode != 0 {
			t.Fatalf("res = %+v", res)
		}
	})

	// Regression: Duration used to come back 0 because the write was
	// deferred against the shadowed result, not the one the caller got.
	t.Run("duration is measured", func(t *testing.T) {
		res := Run(ctx, dir, Spec{Command: "sleep 0.1"})
		if !res.OK {
			t.Fatalf("res = %+v", res)
		}
		if res.Duration < 100*time.Millisecond {
			t.Fatalf("Duration = %v, want >= 100ms", res.Duration)
		}
	})
}

func TestExcerpt(t *testing.T) {
	huge := strings.Repeat("x", 100)
	tests := []struct {
		name     string
		output   string
		maxLines int
		maxBytes int
		want     string
	}{
		{"empty input", "", 5, 100, ""},
		{"whitespace only", " \n\t\n", 5, 100, ""},
		{"keeps the last N non-empty lines", "a\n\nb\nc\n\nd\ne\n", 3, 100, "c\nd\ne"},
		{"trims trailing whitespace", "a  \nb\t\n", 5, 100, "a\nb"},
		{"drops lines from the front to fit maxBytes", "0123456789\n0123456789\n0123456789", 10, 22, "0123456789\n0123456789"},
		{"exact fit keeps everything", "a\nb\nc", 3, 5, "a\nb\nc"},
		{"single huge line keeps its tail with ellipsis", huge, 1, 10, "…" + strings.Repeat("x", 10)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Excerpt(tt.output, tt.maxLines, tt.maxBytes); got != tt.want {
				t.Fatalf("Excerpt(%q, %d, %d) = %q, want %q", tt.output, tt.maxLines, tt.maxBytes, got, tt.want)
			}
		})
	}
}
