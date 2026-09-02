package ui

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestRenderUnderGCStress hammers the render path with the GC running as
// aggressively as possible. The live crash was
// "fatal error: found bad pointer in Go heap", thrown while the GC scanned
// the stack frame of Model.viewChat — a frame ~11KB wide because Model is
// passed by value. Run with GOGC=1 GODEBUG=gccheckmark=1.
func TestRenderUnderGCStress(t *testing.T) {
	if os.Getenv("STRAWBOSS_STRESS") == "" {
		t.Skip("set STRAWBOSS_STRESS=1 (takes ~45s; ~140s under -race)")
	}
	m := New(make(chan tea.Msg))
	var msgs []tea.Msg
	for i := 0; i < 160; i++ {
		id := fmt.Sprintf("w%d", i+1)
		msgs = append(msgs,
			WorkerUpsertMsg{ID: id, Model: "qwen-dsh", Task: strings.Repeat("task words ", 40), Status: "running", Started: time.Now()},
			WorkerUsageMsg{ID: id, Input: 123456, Output: 46500, CacheRead: 900000, Ctx: 90000},
			WorkerEventMsg{ID: id, Kind: "tool", Text: strings.Repeat("tool output ", 60)},
			WorkerEventMsg{ID: id, Kind: "text", Text: strings.Repeat("streamed ", 80)},
			SupTextDeltaMsg{Text: strings.Repeat("supervisor streaming text ", 20)},
			SupUsageMsg{Input: 12, Output: 10193, CacheRead: 14650000, CacheWrite: 792, CostUSD: 0.28, Turns: 1, Ctx: 222700, CtxWindow: 1000000},
			WorkerUpsertMsg{ID: id, Status: "done", Summary: strings.Repeat("summary ", 40)},
		)
	}
	m = apply(t, m, msgs...)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Independent allocators, so the GC is always mid-cycle during renders.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b := make([][]byte, 64)
				for j := range b {
					b[j] = []byte(strings.Repeat("x", 512))
				}
				_ = b
			}
		}()
	}

	for i := 0; i < 4000; i++ {
		w, h := 40+(i%180), 10+(i%50)
		mm := m
		mm.width, mm.height = w, h
		_ = mm.View()
		if i%200 == 0 {
			runtime.GC()
		}
	}
	close(stop)
	wg.Wait()
}
