// Package slots gives worker models cross-process concurrency slots. A
// model that is allowed N concurrent workers gets N lock files in a shared
// directory; a worker grabs one and holds its kernel flock for as long as
// it runs. Because the lock lives in the kernel, slots are shared between
// separate processes (several delegate commands, the TUI's retry path),
// and the kernel drops the lock when the holder dies — a crashed holder
// can never leak a slot.
package slots

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// PollEvery is how often Acquire retries; tests shorten it.
var PollEvery = 500 * time.Millisecond

// safe maps a model name to a file-safe string: any character outside
// [A-Za-z0-9._-] becomes "_".
func safe(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// slotPath is the lock file for slot i of name in dir.
func slotPath(dir, name string, i int) string {
	return filepath.Join(dir, fmt.Sprintf("%s.%d.lock", safe(name), i))
}

// TryAcquire takes a free slot for name among max slots without waiting.
// ok is false when all are busy. max <= 0 means unlimited: it returns a
// no-op release and ok true without touching the filesystem.
func TryAcquire(dir, name string, max int) (release func(), ok bool, err error) {
	if max <= 0 {
		return func() {}, true, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false, fmt.Errorf("slots dir: %w", err)
	}
	for i := 0; i < max; i++ {
		f, err := os.OpenFile(slotPath(dir, name, i), os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, false, fmt.Errorf("slot %d open: %w", i, err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				continue // busy; try the next slot
			}
			return nil, false, fmt.Errorf("slot %d lock: %w", i, err)
		}
		var once sync.Once
		return func() {
			once.Do(func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			})
		}, true, nil
	}
	return nil, false, nil
}

// Busy counts how many of name's max slots are held right now, by probing
// each with a non-blocking lock it releases at once. A snapshot for
// display only: it can be stale the moment it returns. max <= 0 is 0.
func Busy(dir, name string, max int) int {
	busy := 0
	for i := 0; i < max; i++ {
		f, err := os.OpenFile(slotPath(dir, name, i), os.O_RDWR, 0)
		if err != nil {
			continue // never created: never held
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			if errors.Is(err, syscall.EWOULDBLOCK) {
				busy++
			}
		} else {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		}
		f.Close()
	}
	return busy
}

// Acquire waits for a free slot, polling every PollEvery, until ctx is done
// (then it returns ctx's error wrapped with context). max <= 0 as above.
func Acquire(ctx context.Context, dir, name string, max int) (release func(), err error) {
	if max <= 0 {
		return func() {}, nil
	}
	for {
		release, ok, err := TryAcquire(dir, name, max)
		if err != nil {
			return nil, err
		}
		if ok {
			return release, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(PollEvery):
		}
	}
}
