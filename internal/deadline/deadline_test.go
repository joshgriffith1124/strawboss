package deadline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWithTimeoutFires(t *testing.T) {
	start := time.Now()
	ctx, cancel := WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	<-ctx.Done()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("fired after %v; want ~30ms", elapsed)
	}
	// Note: ctx.Err() is context.Canceled even on expiry (Go sets Err to
	// Canceled under WithCancelCause); Cause carries DeadlineExceeded.
	if cause := context.Cause(ctx); !errors.Is(cause, context.DeadlineExceeded) {
		t.Errorf("Cause = %v; want DeadlineExceeded", cause)
	}
}

func TestWithTimeoutExtendLater(t *testing.T) {
	t0 := time.Now()
	ctx, cancel := WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	dl := FromContext(ctx)
	if dl == nil {
		t.Fatal("FromContext = nil; want the Deadline")
	}
	later := time.Now().Add(10 * time.Second)
	if !dl.ExtendTo(later) {
		t.Fatal("ExtendTo later = false; want true")
	}
	if got, _ := ctx.Deadline(); !got.Equal(later) {
		t.Errorf("ctx.Deadline() = %v; want %v", got, later)
	}
	select {
	case <-ctx.Done():
		t.Fatalf("ctx done after %v; want it to keep living", time.Since(t0))
	case <-time.After(80 * time.Millisecond):
	}
	if dl.At() != later {
		t.Errorf("At() = %v; want %v", dl.At(), later)
	}
	// Still cancellable as a plain context.
	cancel()
	<-ctx.Done()
	if cause := context.Cause(ctx); !errors.Is(cause, context.Canceled) {
		t.Errorf("Cause = %v; want Canceled", cause)
	}
}

func TestWithTimeoutExtendEarlierRefused(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), time.Second)
	defer cancel()
	dl := FromContext(ctx)
	before := dl.At()
	if dl.ExtendTo(time.Now().Add(10 * time.Millisecond)) {
		t.Error("ExtendTo earlier = true; want false")
	}
	if dl.At() != before {
		t.Errorf("At() = %v; want unchanged %v", dl.At(), before)
	}
}

func TestExtendToAfterDoneRefused(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	<-ctx.Done()
	if FromContext(ctx).ExtendTo(time.Now().Add(time.Hour)) {
		t.Error("ExtendTo after done = true; want false")
	}
}

func TestFromContextNil(t *testing.T) {
	if got := FromContext(context.Background()); got != nil {
		t.Errorf("FromContext(Background) = %v; want nil", got)
	}
}

func TestWatchPicksUpRequest(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	path := Path(dir, "w1")
	at := time.Now().Add(time.Second).Truncate(time.Second) // RFC3339 precision
	// Request after the watcher starts, so a fast first tick can't race it.
	go func() {
		time.Sleep(5 * time.Millisecond)
		if err := Request(dir, "w1", at); err != nil {
			t.Error(err)
		}
	}()
	extended := make(chan time.Time, 1)
	go func() {
		FromContext(ctx).Watch(ctx, path, 5*time.Millisecond, func(newAt time.Time) { extended <- newAt })
	}()
	select {
	case got := <-extended:
		if !got.Equal(at) {
			t.Errorf("onExtend(%v); want %v", got, at)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onExtend never called")
	}
	if !FromContext(ctx).At().Equal(at) {
		t.Errorf("At() = %v; want %v", FromContext(ctx).At(), at)
	}
}

func TestWatchIgnoresGarbage(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	path := Path(dir, "w2")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a time"), 0o644); err != nil {
		t.Fatal(err)
	}
	extended := false
	done := make(chan struct{})
	go func() {
		FromContext(ctx).Watch(ctx, path, 5*time.Millisecond, func(time.Time) { extended = true })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not stop with ctx")
	}
	if extended {
		t.Error("onExtend called for garbage file")
	}
}

func TestRequestAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Add(3 * time.Minute).Truncate(time.Second)
	if err := Request(dir, "w9", at); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(Path(dir, "w9"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := time.Parse(time.RFC3339, string(b))
	if err != nil {
		t.Fatalf("file %q: %v", b, err)
	}
	if !got.Equal(at) {
		t.Errorf("got %v; want %v", got, at)
	}
	if _, err := os.Stat(filepath.Dir(Path(dir, "w9"))); err != nil {
		t.Fatalf("deadlines dir: %v", err)
	}
	// A second request overwrites in place.
	if err := Request(dir, "w9", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
}
