// Package deadline implements a worker's per-attempt timeout that an
// operator can push back while the attempt is running. WithTimeout wraps
// context.WithTimeout: the returned ctx still expires (and still reads
// "context deadline exceeded" via context.Cause), but its deadline is a
// *Deadline object reachable through FromContext, so a later attempt can
// ExtendTo a new time before the old one fires.
package deadline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Deadline is a mutable deadline attached to a worker attempt. Safe for
// concurrent use.
type Deadline struct {
	mu     sync.Mutex
	at     time.Time
	timer  *time.Timer
	ctx    context.Context // the withTimeout ctx; done ⇒ no more extensions
	cancel context.CancelCauseFunc
}

// deadlineCtx reports the *current* deadline from Deadline() so callers that
// inspect ctx.Deadline() see extensions, not the original one.
type deadlineCtx struct {
	context.Context
	d *Deadline
}

func (c deadlineCtx) Deadline() (time.Time, bool) {
	if c.Context.Err() != nil {
		return time.Time{}, false
	}
	return c.d.At(), true
}

// ctxKey finds the *Deadline from ctxs derived from a deadlineCtx too.
type ctxKey struct{}

func (c deadlineCtx) Value(key any) any {
	if key == (ctxKey{}) {
		return c.d
	}
	return c.Context.Value(key)
}

// WithTimeout is context.WithTimeout whose deadline can be extended: the
// returned ctx carries the *Deadline (FromContext), and its Deadline() tracks
// extensions. On expiry the ctx is cancelled with cause
// context.DeadlineExceeded, so context.Cause(ctx) reads the same as
// context.WithTimeout's. The returned CancelFunc stops the timer and cancels
// with context.Canceled.
func WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	dl := &Deadline{ctx: ctx, cancel: cancel, at: time.Now().Add(d)}
	dl.timer = time.AfterFunc(d, dl.fire)
	return deadlineCtx{Context: ctx, d: dl}, dl.stop
}

// fire cancels with DeadlineExceeded, ignoring stale firings (an ExtendTo
// reset always moves the deadline later, so an old timer fires early and
// finds time still left).
func (d *Deadline) fire() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if remaining := time.Until(d.at); remaining > 0 || d.ctx.Err() != nil {
		return
	}
	d.cancel(context.DeadlineExceeded)
}

// stop is the CancelFunc: no more timer, cancel as plain cancellation.
func (d *Deadline) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.timer.Stop()
	d.cancel(context.Canceled)
}

// At is the current deadline.
func (d *Deadline) At() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.at
}

// ExtendTo moves the deadline to t if t is later than the current one and
// the ctx is not already done; reports whether it moved.
func (d *Deadline) ExtendTo(t time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !t.After(d.at) || d.ctx.Err() != nil {
		return false
	}
	d.at = t
	d.timer.Reset(time.Until(t))
	return true
}

// FromContext returns the *Deadline carried by ctx, or nil when ctx has
// none (e.g. a plain context.Background()). ctxs derived from a WithTimeout
// ctx carry it too.
func FromContext(ctx context.Context) *Deadline {
	d, _ := ctx.Value(ctxKey{}).(*Deadline)
	return d
}

// Path is where an extension request for a worker lives:
// <stateDir>/deadlines/<workerID>.
func Path(stateDir, workerID string) string {
	return filepath.Join(stateDir, "deadlines", workerID)
}

// Request writes an extension request for workerID — the absolute time in
// RFC3339 — atomically (temp file + rename), creating the directory.
func Request(stateDir, workerID string, t time.Time) error {
	dir := filepath.Join(stateDir, "deadlines")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("deadline dir: %w", err)
	}
	path := Path(stateDir, workerID)
	tmp, err := os.CreateTemp(dir, workerID+".tmp-*")
	if err != nil {
		return fmt.Errorf("deadline temp: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(t.Format(time.RFC3339)); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("deadline write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("deadline write: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("deadline rename: %w", err)
	}
	return nil
}

// Watch polls path every interval until ctx is done; when the file holds an
// RFC3339 time later than At(), it calls ExtendTo and then onExtend(newAt).
// A missing or unparseable file is a no-op. Blocks; run it in a goroutine.
func (d *Deadline) Watch(ctx context.Context, path string, interval time.Duration, onExtend func(time.Time)) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			b, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			t, err := time.Parse(time.RFC3339, string(b))
			if err != nil {
				continue
			}
			if d.ExtendTo(t) {
				onExtend(d.At())
			}
		}
	}
}
