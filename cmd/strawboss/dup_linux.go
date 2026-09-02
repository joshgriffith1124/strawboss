//go:build linux

package main

import (
	"os"
	"syscall"
)

// dupToStderr makes fd 2 a copy of f. Linux has no dup2 on every
// architecture (arm64 dropped it); dup3 with no flags is equivalent.
func dupToStderr(f *os.File) error {
	return syscall.Dup3(int(f.Fd()), int(os.Stderr.Fd()), 0)
}
