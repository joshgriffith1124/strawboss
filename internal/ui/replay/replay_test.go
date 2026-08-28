package replay

import (
	"testing"
	"time"

	"strawboss/internal/ui"
)

// TestTimeline drains the replay feed at high speed and checks the shape:
// init first, delegations create workers, a failure occurs, recorded
// worker events and supervisor stream events come through.
func TestTimeline(t *testing.T) {
	feed := Feed(200)
	timeout := time.After(30 * time.Second)

	var sawInit, sawDelegate, sawFailed, sawWorkerEvent, sawSupDelta, sawUsage bool
	first := true
	for !(sawInit && sawDelegate && sawFailed && sawWorkerEvent && sawSupDelta && sawUsage) {
		select {
		case msg := <-feed:
			if first {
				if _, ok := msg.(ui.SupInitMsg); !ok {
					t.Fatalf("first msg = %#v, want SupInitMsg", msg)
				}
				first = false
			}
			switch m := msg.(type) {
			case ui.SupInitMsg:
				sawInit = true
				if m.Auth != "subscription" {
					t.Errorf("auth = %q", m.Auth)
				}
			case ui.SupToolMsg:
				if m.Delegate != nil {
					sawDelegate = true
				}
			case ui.WorkerUpsertMsg:
				if m.Status == "failed" {
					sawFailed = true
					if m.ID != "w4" {
						t.Errorf("failed worker = %s, want w4", m.ID)
					}
				}
			case ui.WorkerEventMsg:
				sawWorkerEvent = true
			case ui.SupTextDeltaMsg:
				sawSupDelta = true
			case ui.SupUsageMsg:
				if m.CostUSD > 0 {
					sawUsage = true
				}
			}
		case <-timeout:
			t.Fatalf("timeline incomplete: init=%v delegate=%v failed=%v workerEvent=%v supDelta=%v usage=%v",
				sawInit, sawDelegate, sawFailed, sawWorkerEvent, sawSupDelta, sawUsage)
		}
	}
}
