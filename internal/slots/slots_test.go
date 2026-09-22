package slots

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func TestTryAcquireUpToMax(t *testing.T) {
	dir := t.TempDir()

	r1, ok, err := TryAcquire(dir, "model-a", 2)
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	r2, ok, err := TryAcquire(dir, "model-a", 2)
	if err != nil || !ok {
		t.Fatalf("second acquire: ok=%v err=%v", ok, err)
	}
	if _, ok, err := TryAcquire(dir, "model-a", 2); err != nil || ok {
		t.Fatalf("third acquire: ok=%v err=%v, want ok=false", ok, err)
	}

	r1()
	r3, ok, err := TryAcquire(dir, "model-a", 2)
	if err != nil || !ok {
		t.Fatalf("acquire after release: ok=%v err=%v", ok, err)
	}
	r2()
	r3()
}

func TestUnlimitedCreatesNoFiles(t *testing.T) {
	dir := t.TempDir()

	release, ok, err := TryAcquire(dir, "model-a", 0)
	if err != nil || !ok {
		t.Fatalf("acquire max=0: ok=%v err=%v", ok, err)
	}
	release()
	release() // no-op release, safe to call twice

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unlimited acquire created %d files in %s, want none", len(entries), dir)
	}
}

func TestAcquireWaitsForRelease(t *testing.T) {
	dir := t.TempDir()

	hold, ok, err := TryAcquire(dir, "model-a", 1)
	if err != nil || !ok {
		t.Fatalf("hold: ok=%v err=%v", ok, err)
	}

	old := PollEvery
	PollEvery = 10 * time.Millisecond
	var wg sync.WaitGroup
	wg.Add(1)
	t.Cleanup(func() {
		wg.Wait() // stop the goroutine before restoring PollEvery
		PollEvery = old
	})

	type res struct {
		release func()
		err     error
	}
	done := make(chan res, 1)
	go func() {
		defer wg.Done()
		rel, err := Acquire(context.Background(), dir, "model-a", 1)
		done <- res{rel, err}
	}()

	// Let the wait loop block a few times before the slot frees up.
	time.Sleep(50 * time.Millisecond)
	hold()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Acquire: %v", r.err)
		}
		r.release()
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return within 2s of release")
	}
}

func TestAcquireRespectsContext(t *testing.T) {
	dir := t.TempDir()

	hold, ok, err := TryAcquire(dir, "model-a", 1)
	if err != nil || !ok {
		t.Fatalf("hold: ok=%v err=%v", ok, err)
	}
	defer hold()

	old := PollEvery
	PollEvery = 10 * time.Millisecond
	t.Cleanup(func() { PollEvery = old })

	// Already-cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Acquire(ctx, dir, "model-a", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: err=%v, want context.Canceled", err)
	}

	// Short deadline while the slot is held.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	if _, err := Acquire(ctx2, dir, "model-a", 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout ctx: err=%v, want context.DeadlineExceeded", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	dir := t.TempDir()

	release, ok, err := TryAcquire(dir, "model-a", 1)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	release()
	release() // must not panic

	// The slot is actually free again.
	if _, ok, err := TryAcquire(dir, "model-a", 1); err != nil || !ok {
		t.Fatalf("reacquire after double release: ok=%v err=%v", ok, err)
	}
}

func TestUnsafeNameMapsToSafeFile(t *testing.T) {
	dir := t.TempDir()

	release, ok, err := TryAcquire(dir, "weird / name v2", 1)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	release()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly 1 file in %s, got %d", dir, len(entries))
	}
	if entries[0].IsDir() {
		t.Fatalf("slot entry %q is a directory, want a file (no subdirectory created)", entries[0].Name())
	}
	if entries[0].Name() != "weird___name_v2.0.lock" {
		t.Fatalf("slot file = %q, want %q", entries[0].Name(), "weird___name_v2.0.lock")
	}
}
