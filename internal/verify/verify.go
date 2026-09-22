// Package verify is strawboss's verify gate. Local workers sometimes report
// success they did not earn, and the supervisor cannot run builds or tests
// itself, so strawboss runs a project-defined check command after a worker
// finishes and trusts that instead of the worker's word. This package finds
// the project's check, runs it, and condenses its output into a few lines
// that fit the terse-result budget.
package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// FileName is the per-project settings file, looked up from the working
// directory upward.
const FileName = ".strawboss.toml"

// defaultTimeout applies when .strawboss.toml leaves verify_timeout unset.
const defaultTimeout = 5 * time.Minute

// outputCap is how much check output Result.Output keeps: the tail.
const outputCap = 64 * 1024

// Spec is a project's verify settings.
type Spec struct {
	// Command is the shell command run with sh -c in the worker's
	// directory; empty = no check.
	Command string
	// Timeout defaults to 5m when the file leaves it unset.
	Timeout time.Duration
	// Source is the path of the file it came from ("" when none was found).
	Source string
}

// Result is one run of the check.
type Result struct {
	OK bool
	// ExitCode is -1 when the command could not start or was killed by
	// the timeout.
	ExitCode int
	TimedOut bool
	// Output is the combined stdout+stderr, capped at the last 64 KiB.
	Output   string
	Duration time.Duration
}

// file is the on-disk shape of .strawboss.toml:
//
//	verify         = "go test ./..."
//	verify_timeout = "10m"      # optional, parsed with time.ParseDuration
type file struct {
	Verify        string `toml:"verify"`
	VerifyTimeout string `toml:"verify_timeout"`
}

// Find walks from dir upward (dir, its parent, …) and loads the first
// FileName it finds; it stops after the directory that contains a .git
// entry (file or dir), or at the filesystem root. No file anywhere is not
// an error: it returns a zero Spec.
func Find(dir string) (Spec, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Spec{}, fmt.Errorf("resolving %s: %w", dir, err)
	}
	for {
		spec, err := loadFile(filepath.Join(abs, FileName))
		if err != nil {
			return Spec{}, err
		}
		if spec.Source != "" {
			return spec, nil
		}
		// .git marks the project root; anything above it belongs to
		// whatever repository embeds this one, so stop here.
		if _, err := os.Stat(filepath.Join(abs, ".git")); err == nil {
			return Spec{}, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return Spec{}, nil
		}
		abs = parent
	}
}

// loadFile reads one candidate path. A missing file is not an error.
func loadFile(path string) (Spec, error) {
	var f file
	meta, err := toml.DecodeFile(path, &f)
	if os.IsNotExist(err) {
		return Spec{}, nil
	}
	if err != nil {
		return Spec{}, fmt.Errorf("loading %s: %w", path, err)
	}
	if undec := meta.Undecoded(); len(undec) > 0 {
		return Spec{}, fmt.Errorf("loading %s: unknown key %q", path, undec[0].String())
	}
	spec := Spec{Command: f.Verify, Timeout: defaultTimeout, Source: path}
	if f.VerifyTimeout != "" {
		d, err := time.ParseDuration(f.VerifyTimeout)
		if err != nil {
			return Spec{}, fmt.Errorf("loading %s: verify_timeout %q: %w", path, f.VerifyTimeout, err)
		}
		if d < 0 {
			return Spec{}, fmt.Errorf("loading %s: verify_timeout %q must not be negative", path, f.VerifyTimeout)
		}
		spec.Timeout = d
	}
	return spec, nil
}

// Run executes spec.Command with `sh -c` in dir, with spec.Timeout (5m if
// zero) on top of ctx. The command's process group is killed on timeout
// (cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} plus kill -pgid)
// so test runners that fork children do not linger. An empty Command is a
// vacuous pass: with no check defined, the gate has nothing to object to.
func Run(ctx context.Context, dir string, spec Spec) (res Result) {
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	start := time.Now()
	res = Result{ExitCode: -1}
	// Named result: the deferred write must land in what the caller gets.
	defer func() { res.Duration = time.Since(start) }()
	if spec.Command == "" {
		res.OK = true
		res.ExitCode = 0
		return res
	}

	cmd := exec.Command("sh", "-c", spec.Command)
	cmd.Dir = dir
	// Its own process group, so a timeout can kill the runner and its
	// children at once, not just the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	w := &lockedWriter{b: &out}
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		res.Output = "start: " + err.Error()
		return res
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	timedOut := false
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		killGroup(cmd)
		err = <-done
	case <-timer.C:
		timedOut = true
		killGroup(cmd)
		err = <-done
	}

	res.TimedOut = timedOut
	res.Output = capOutput(out.String())
	if err == nil {
		res.OK = true
		res.ExitCode = 0
		return res
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
	}
	return res
}

// killGroup SIGKILLs the command's whole process group.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// capOutput keeps the last outputCap bytes of s, backed up to a UTF-8
// rune boundary.
func capOutput(s string) string {
	if len(s) <= outputCap {
		return s
	}
	start := len(s) - outputCap
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// lockedWriter serializes exec's stdout/stderr copy goroutines onto one
// buffer.
type lockedWriter struct {
	mu sync.Mutex
	b  *bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// Excerpt condenses check output for a terse result: the last maxLines
// non-empty lines, each trimmed of trailing whitespace, joined with "\n",
// then cut to at most maxBytes by dropping whole lines from the FRONT
// (keep the end — failures print last). If a single remaining line is
// still longer than maxBytes, keep its last maxBytes bytes, prefixed with
// "…".
func Excerpt(output string, maxLines, maxBytes int) string {
	if maxLines <= 0 || maxBytes <= 0 {
		return ""
	}
	lines := make([]string, 0, 8)
	for _, l := range strings.Split(output, "\n") {
		if l = strings.TrimRightFunc(l, unicode.IsSpace); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	n := len(lines)
	if n == 0 {
		return ""
	}
	// Drop whole lines from the front until the joined tail fits maxBytes.
	start := 0
	for start < n {
		total := 0
		for i := start; i < n; i++ {
			if i > start {
				total++ // the separating "\n"
			}
			total += len(lines[i])
		}
		if total <= maxBytes {
			break
		}
		start++
	}
	if start < n {
		return strings.Join(lines[start:], "\n")
	}
	// Not even the last line fits whole.
	last := lines[n-1]
	cut := len(last) - maxBytes
	for cut < len(last) && !utf8.RuneStart(last[cut]) {
		cut++ // never split a rune
	}
	return "…" + last[cut:]
}
