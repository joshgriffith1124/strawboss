//go:build darwin

package main

import (
	"os"
	"syscall"
)

// dupToStderr makes fd 2 a copy of f.
func dupToStderr(f *os.File) error {
	return syscall.Dup2(int(f.Fd()), int(os.Stderr.Fd()))
}
