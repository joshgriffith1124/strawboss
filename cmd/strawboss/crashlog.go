package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// captureStderr points file descriptor 2 at a file for the life of the
// TUI, and returns a function that closes it — reporting the path if
// anything was written.
//
// This is fd-level on purpose. A Go runtime fatal error (concurrent map
// writes, out of memory, a deadlock) is not a panic: recover() cannot see
// it, and the runtime writes the traceback straight to fd 2. Reassigning
// os.Stderr would miss it entirely. Redirecting the descriptor catches
// ordinary panics and runtime fatals alike.
//
// Losing stderr costs nothing here — anything written to it during an
// alt-screen TUI would corrupt the display rather than being read.
func captureStderr(stateDir string) (func(), error) {
	path := filepath.Join(stateDir, fmt.Sprintf("crash-%d.log", os.Getpid()))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening crash log: %w", err)
	}
	if err := dupToStderr(f); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("redirecting stderr: %w", err)
	}
	fmt.Fprintf(f, "strawboss %s pid %d — stderr capture opened\n",
		time.Now().Format(time.RFC3339), os.Getpid())
	header, _ := f.Seek(0, 1) // bytes written above; anything past this is real

	return func() {
		n, _ := f.Seek(0, 2)
		f.Close()
		if n <= header {
			os.Remove(path) // clean run: leave no litter
			return
		}
		// The TUI has given the terminal back by now, so this is visible.
		fmt.Fprintf(os.Stdout, "\nstrawboss wrote %d bytes to stderr — see %s\n", n-header, path)
	}, nil
}
