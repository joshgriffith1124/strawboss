package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAllocateSequentialAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workers.jsonl")
	// Two instances on the same file, as two delegate processes would be.
	a := &Registry{Path: path}
	b := &Registry{Path: path}

	id1, err := a.Allocate("ses_1", "qwen-coder", "task one", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := b.Allocate("ses_2", "qwen-coder", "task two", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != "w1" || id2 != "w2" {
		t.Errorf("ids = %s, %s; want w1, w2", id1, id2)
	}
}

func TestAllocateConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workers.jsonl")
	const n = 20
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &Registry{Path: path}
			id, err := r.Allocate(fmt.Sprintf("ses_%d", i), "m", "t", "/")
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = id
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate worker id %s in %v", id, ids)
		}
		seen[id] = true
	}
}

func TestFinishLoadReduce(t *testing.T) {
	r := &Registry{Path: filepath.Join(t.TempDir(), "workers.jsonl")}
	w1, _ := r.Allocate("ses_1", "qwen-coder", "build the thing", "/repo")
	w2, _ := r.Allocate("ses_2", "qwen-small", "docstrings", "/repo")
	if err := r.Finish(w1, "ses_1", "done", "built; tests pass", "/logs/ses_1.jsonl", 95*time.Second, 12000, 800); err != nil {
		t.Fatal(err)
	}

	events, err := r.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events", len(events))
	}
	workers := Reduce(events)
	if len(workers) != 2 {
		t.Fatalf("got %d workers", len(workers))
	}
	got1, got2 := workers[0], workers[1]
	if got1.ID != w1 || got1.Status != "done" || got1.Summary != "built; tests pass" {
		t.Errorf("w1 = %+v", got1)
	}
	if got1.InputTokens != 12000 || got1.OutputTokens != 800 {
		t.Errorf("w1 tokens = %d/%d", got1.InputTokens, got1.OutputTokens)
	}
	if got1.LogPath != "/logs/ses_1.jsonl" {
		t.Errorf("w1 log = %q", got1.LogPath)
	}
	if got2.ID != w2 || got2.Status != "running" {
		t.Errorf("w2 = %+v", got2)
	}
	if got2.Task != "docstrings" || got2.Model != "qwen-small" {
		t.Errorf("w2 = %+v", got2)
	}
}

func TestLoadMissingAndCorrupt(t *testing.T) {
	r := &Registry{Path: filepath.Join(t.TempDir(), "nope.jsonl")}
	events, err := r.Load()
	if err != nil || events != nil {
		t.Fatalf("missing file: events=%v err=%v", events, err)
	}

	path := filepath.Join(t.TempDir(), "workers.jsonl")
	content := `{"ts":"2026-08-28T12:00:00Z","type":"spawned","worker":"w1","session":"s1"}
this line is garbage
{"ts":"2026-08-28T12:01:00Z","type":"finished","worker":"w1","session":"s1","status":"done"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	r = &Registry{Path: path}
	events, err = r.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (garbage skipped)", len(events))
	}
	workers := Reduce(events)
	if len(workers) != 1 || workers[0].Status != "done" {
		t.Errorf("workers = %+v", workers)
	}
}

func TestReduceFinishedWithoutSpawn(t *testing.T) {
	// A finished event for an unknown worker (truncated log) is ignored.
	workers := Reduce([]Event{{Type: "finished", Worker: "w9", Status: "done"}})
	if len(workers) != 0 {
		t.Errorf("workers = %+v", workers)
	}
}
