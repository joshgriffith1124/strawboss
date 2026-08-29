package opencode

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/harness"
)

// TestLiveWorker proves the harness against a real `opencode serve` and a
// real inference endpoint. Skipped unless STRAWBOSS_OPENCODE_URL is set
// (e.g. http://127.0.0.1:4477); STRAWBOSS_OPENCODE_MODEL overrides the
// default spark-a/qwen3.8-27b.
func TestLiveWorker(t *testing.T) {
	base := os.Getenv("STRAWBOSS_OPENCODE_URL")
	if base == "" {
		t.Skip("STRAWBOSS_OPENCODE_URL not set")
	}
	model := os.Getenv("STRAWBOSS_OPENCODE_MODEL")
	if model == "" {
		model = "spark-a/qwen3.8-27b"
	}
	mc := config.ModelConfig{Name: "live", Endpoint: base, Model: model, Harness: "opencode"}
	h := New(mc, t.TempDir(), t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	id, err := h.Spawn(ctx, "Reply with exactly: live harness ok. Do not use any tools.", mc)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("worker %s spawned", id)

	events, err := h.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			if ev.Kind == "text" {
				streamed.WriteString(ev.Text)
			}
		}
	}()

	res, err := h.Result(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	<-done
	t.Logf("status=%s summary=%q log=%s streamed=%q", res.Status, res.Summary, res.LogPath, streamed.String())

	if res.Status != harness.StatusDone {
		t.Errorf("status = %q", res.Status)
	}
	if !strings.Contains(res.Summary, "live harness ok") {
		t.Errorf("summary = %q", res.Summary)
	}
	if _, err := os.Stat(res.LogPath); err != nil {
		t.Errorf("log path: %v", err)
	}
	u, err := h.Usage(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if u.InputTokens == 0 || u.OutputTokens == 0 {
		t.Errorf("usage = %+v, want nonzero", u)
	}
	t.Logf("usage: %+v", u)
}
