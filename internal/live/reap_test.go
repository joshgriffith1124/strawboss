package live

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/registry"
	"github.com/joshgriffith1124/strawboss/internal/supervisor"
	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// installReapFakes swaps the bootTime/pidExists/killWorker vars for fakes,
// restoring the originals on cleanup.
func installReapFakes(t *testing.T, boot time.Time, alive map[int]bool, kills *[]int) {
	t.Helper()
	oldBoot, oldAlive, oldKill := bootTime, pidExists, killWorker
	t.Cleanup(func() { bootTime, pidExists, killWorker = oldBoot, oldAlive, oldKill })
	bootTime = func() time.Time { return boot }
	pidExists = func(pid int) bool { return alive[pid] }
	killWorker = func(pid int) error {
		if kills != nil {
			*kills = append(*kills, pid)
		}
		return nil
	}
}

func TestWorkerAlive(t *testing.T) {
	boot := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		boot    time.Time // zero disables the boot check
		alive   map[int]bool
		pid     int
		started time.Time
		want    bool
	}{
		{"unknown pid is alive", boot, map[int]bool{}, 0, boot.Add(time.Hour), true},
		{"pid gone is dead", boot, map[int]bool{}, 4242, boot.Add(time.Hour), false},
		{"pid predating the last boot is dead even though it exists", boot, map[int]bool{4242: true}, 4242, boot.Add(-time.Hour), false},
		{"zero boot time disables the boot check", time.Time{}, map[int]bool{4242: true}, 4242, boot.Add(-time.Hour), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installReapFakes(t, tt.boot, tt.alive, nil)
			if got := workerAlive(tt.pid, tt.started); got != tt.want {
				t.Errorf("workerAlive(%d, %s) = %v, want %v", tt.pid, tt.started.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

// TestReapDead: at startup, provably dead workers are closed out as failed;
// live, post-boot, and already-finished rows stay untouched.
func TestReapDead(t *testing.T) {
	stateDir := t.TempDir()
	regPath := filepath.Join(stateDir, "workers.jsonl")
	boot := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	before, after := boot.Add(-time.Hour), boot.Add(time.Hour)

	// w1 dsh, pid gone; w2 dsh, pid alive; w3 opencode started pre-boot;
	// w4 opencode started post-boot; w5 already finished.
	seed := []registry.Event{
		{TS: after, Type: "spawned", Worker: "w1", Session: "ses1", Model: "m-dsh", Task: "t", Dir: "/repo", PID: 9001},
		{TS: after, Type: "spawned", Worker: "w2", Session: "ses2", Model: "m-dsh", Task: "t", Dir: "/repo", PID: 9002},
		{TS: before, Type: "spawned", Worker: "w3", Session: "ses3", Model: "m-oc", Task: "t", Dir: "/repo"},
		{TS: after, Type: "spawned", Worker: "w4", Session: "ses4", Model: "m-oc", Task: "t", Dir: "/repo"},
		{TS: after, Type: "spawned", Worker: "w5", Session: "ses5", Model: "m-oc", Task: "t", Dir: "/repo"},
		{TS: after.Add(time.Second), Type: "finished", Worker: "w5", Session: "ses5", Status: "done", Summary: "ok"},
	}
	f, err := os.OpenFile(regPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range seed {
		if err := json.NewEncoder(f).Encode(ev); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
	}
	_ = f.Close()

	installReapFakes(t, boot, map[int]bool{9002: true}, nil)
	o := New(&supervisor.Driver{}, nil, stateDir)
	o.reapDead()

	events, err := (&registry.Registry{Path: regPath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]registry.Worker{}
	for _, w := range registry.Reduce(events) {
		byID[w.ID] = w
	}
	for _, want := range []struct {
		worker, status string
		summary        string
	}{
		{"w1", "failed", reapSummary},
		{"w2", "running", ""},
		{"w3", "failed", reapSummary},
		{"w4", "running", ""},
		{"w5", "done", "ok"},
	} {
		t.Run(want.worker, func(t *testing.T) {
			w := byID[want.worker]
			if w.Status != want.status || w.Summary != want.summary {
				t.Errorf("%s = %s/%q, want %s/%q", want.worker, w.Status, w.Summary, want.status, want.summary)
			}
		})
	}

	// o.started is re-stamped after the reap events are written, so they
	// replay silent (no bell, no push).
	for _, id := range []string{"w1", "w3"} {
		for _, ev := range events {
			if ev.Worker == id && ev.Type == "finished" && ev.TS.After(o.started) {
				t.Errorf("%s reap event %s is after o.started %s — it would replay loud", id, ev.TS, o.started)
			}
		}
	}
}

// TestOnKillWorker: a dead dsh worker is reaped (its pid is never signalled);
// a live one is signalled exactly once.
func TestOnKillWorker(t *testing.T) {
	const pid = 4242
	models := []config.ModelConfig{{Name: "m-dsh", Endpoint: "http://fake:1/v1", Model: "x", Harness: "dsh"}}

	newOrch := func(t *testing.T) *Orchestrator {
		t.Helper()
		o := New(&supervisor.Driver{}, models, t.TempDir())
		o.mu.Lock()
		o.workerSession["w1"] = "ses_x"
		o.workerModel["w1"] = "m-dsh"
		o.workerPID["w1"] = pid
		o.workerStarted["w1"] = time.Now()
		o.unfinished["w1"] = true
		o.mu.Unlock()
		return o
	}

	t.Run("dead worker is reaped, never signalled", func(t *testing.T) {
		var kills []int
		installReapFakes(t, time.Time{}, map[int]bool{}, &kills) // boot check off; pid gone
		o := newOrch(t)

		o.OnKillWorker("w1")
		drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
			tm, ok := m.(ui.ToastMsg)
			return ok && strings.Contains(tm.Text, "already dead")
		})
		if len(kills) != 0 {
			t.Errorf("killWorker called %d times, want 0", len(kills))
		}
		events, err := (&registry.Registry{Path: filepath.Join(o.StateDir, "workers.jsonl")}).Load()
		if err != nil {
			t.Fatal(err)
		}
		got := false
		for _, ev := range events {
			if ev.Worker == "w1" && ev.Type == "finished" && ev.Status == "failed" && ev.Summary == reapSummary {
				got = true
			}
		}
		if !got {
			t.Errorf("no reaped finished event for w1: %+v", events)
		}
		o.mu.Lock()
		marked := o.reaped["w1"]
		o.mu.Unlock()
		if !marked {
			t.Error("w1 not marked reaped — its finished event would replay loud")
		}
	})

	t.Run("live worker is signalled once", func(t *testing.T) {
		var kills []int
		installReapFakes(t, time.Time{}, map[int]bool{pid: true}, &kills)
		o := newOrch(t)

		o.OnKillWorker("w1")
		drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
			tm, ok := m.(ui.ToastMsg)
			return ok && strings.Contains(tm.Text, "killed")
		})
		if len(kills) != 1 || kills[0] != pid {
			t.Errorf("killWorker calls = %v, want [%d]", kills, pid)
		}
	})
}
