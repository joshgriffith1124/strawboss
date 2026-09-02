package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCaptureStderrRemovesCleanLog: a run that writes nothing to stderr
// must not litter the state dir with an empty crash log.
func TestCaptureStderrRemovesCleanLog(t *testing.T) {
	dir := t.TempDir()
	saved, err := os.Open("/dev/stderr")
	if err != nil {
		t.Skipf("no /dev/stderr: %v", err)
	}
	defer saved.Close()

	done, err := captureStderr(dir)
	if err != nil {
		t.Fatalf("captureStderr: %v", err)
	}
	done()
	_ = dupToStderr(saved) // put the real stderr back for the rest of the run

	found, _ := filepath.Glob(filepath.Join(dir, "crash-*.log"))
	if len(found) != 0 {
		t.Errorf("clean run left %v, want no crash log", found)
	}
}

// TestCaptureStderrKeepsRuntimeFatal is the case that matters: a runtime
// fatal error is not a panic, recover() cannot see it, and the runtime
// writes the traceback straight to fd 2. Only an fd-level redirect keeps
// it. Run in a subprocess, since the fatal error kills the process.
func TestCaptureStderrKeepsRuntimeFatal(t *testing.T) {
	if os.Getenv("STRAWBOSS_CRASH_CHILD") == "1" {
		dir := os.Getenv("STRAWBOSS_CRASH_DIR")
		if _, err := captureStderr(dir); err != nil {
			os.Exit(3)
		}
		// Concurrent map writes: a runtime throw, not a panic.
		m := map[int]int{}
		for i := 0; i < 8; i++ {
			go func() {
				for {
					m[1] = 1
				}
			}()
		}
		select {} // the runtime kills us; the traceback goes to the log
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestCaptureStderrKeepsRuntimeFatal")
	cmd.Env = append(os.Environ(), "STRAWBOSS_CRASH_CHILD=1", "STRAWBOSS_CRASH_DIR="+dir)
	_ = cmd.Run() // expected to die

	found, _ := filepath.Glob(filepath.Join(dir, "crash-*.log"))
	if len(found) != 1 {
		t.Fatalf("got %v crash logs, want 1", len(found))
	}
	body, err := os.ReadFile(found[0])
	if err != nil {
		t.Fatalf("reading crash log: %v", err)
	}
	if !strings.Contains(string(body), "fatal error:") {
		t.Errorf("crash log has no fatal error line; got:\n%s", string(body)[:min(len(body), 400)])
	}
	if !strings.Contains(string(body), "goroutine") {
		t.Error("crash log has no goroutine traceback")
	}
}
