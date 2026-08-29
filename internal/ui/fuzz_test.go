package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestViewNeverPanics renders every tab at a sweep of terminal sizes with
// busy state — a View panic at some odd width must be caught here, not in
// a live terminal.
func TestViewNeverPanics(t *testing.T) {
	m := New(make(chan tea.Msg))
	msgs := []tea.Msg{
		SupInitMsg{SessionID: "0123456789abcdef", Model: "claude-fable-5", Auth: "subscription", PID: 4177},
		SupRateLimitMsg{FiveHour: 0.42, SevenDay: 0.9},
		SupUserMsg{Text: strings.Repeat("a long user prompt with words ", 20), Time: time.Now()},
		SupTextDeltaMsg{Text: strings.Repeat("streaming text ", 30)},
		SupStatusMsg{Status: "reviewing a very long status line that should be clipped somewhere sensible…"},
		SupUsageMsg{Input: 1200, Output: 400, CacheRead: 141000, CacheWrite: 7200, CostUSD: 0.17, Turns: 3},
		ModelStatMsg{Name: "a-model-with-a-really-long-name", TokSec: 74, Active: 3, Queue: 2, Note: strings.Repeat("note ", 20)},
	}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("w%d", i+1)
		status := []string{"running", "done", "failed", "queued"}[i%4]
		msgs = append(msgs,
			WorkerUpsertMsg{ID: id, Model: "qwen-coder", Task: strings.Repeat("task words ", 15), Status: "running", Started: time.Now()},
			WorkerUsageMsg{ID: id, Input: 123456, Output: 7890},
			WorkerEventMsg{ID: id, Kind: "tool", Text: strings.Repeat("tool output ", 25)},
			WorkerUpsertMsg{ID: id, Status: status, Summary: strings.Repeat("summary ", 30)},
		)
	}
	msgs = append(msgs, SupToolResultMsg{ToolID: "t", Content: strings.Repeat("result ", 40), IsError: true})
	m = apply(t, m, msgs...)

	for _, w := range []int{0, 1, 5, 10, 20, 30, 39, 40, 45, 52, 54, 60, 79, 80, 100, 120, 200, 400} {
		for _, h := range []int{0, 1, 3, 5, 10, 24, 40, 100} {
			for tab := 0; tab < 3; tab++ {
				m.width, m.height, m.tab = w, h, tab
				for sel := 0; sel < 3; sel++ {
					m.selected = sel * 5
					func() {
						defer func() {
							if r := recover(); r != nil {
								t.Fatalf("View panicked at w=%d h=%d tab=%d sel=%d: %v", w, h, tab, m.selected, r)
							}
						}()
						_ = m.View()
					}()
				}
			}
		}
	}
}
