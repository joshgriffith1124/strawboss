// Package replay is the M4 fake event source: it drives the TUI from
// recorded M1/M2 streams (a real captured supervisor stream-json turn and a
// real captured opencode /global/event SSE stream) plus a scripted
// delegation choreography matching docs/MOCKUP.html — so UI work needs no
// GX10 time and no plan tokens.
package replay

import (
	"bufio"
	_ "embed"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"strawboss/internal/harness/opencode"
	"strawboss/internal/supervisor"
	"strawboss/internal/ui"
)

//go:embed testdata/sup_turn.jsonl
var supTurn []byte

//go:embed testdata/worker_events.sse
var workerEvents string

// Feed returns a channel replaying the demo timeline. speed scales delays
// (2 = twice as fast). The channel stays open; the timeline ends in a
// steady state with the run clock still ticking.
func Feed(speed float64) <-chan tea.Msg {
	if speed <= 0 {
		speed = 1
	}
	ch := make(chan tea.Msg, 16)
	go run(ch, speed)
	return ch
}

func run(ch chan<- tea.Msg, speed float64) {
	sleep := func(d time.Duration) {
		time.Sleep(time.Duration(float64(d) / speed))
	}
	emit := func(msgs ...tea.Msg) {
		for _, m := range msgs {
			ch <- m
		}
	}
	// stream types text out in word chunks like the real partial stream.
	stream := func(text string) {
		words := strings.Fields(text)
		for i := 0; i < len(words); i += 3 {
			end := min(i+3, len(words))
			chunk := strings.Join(words[i:end], " ")
			if end < len(words) {
				chunk += " "
			}
			emit(ui.SupTextDeltaMsg{Text: chunk})
			sleep(90 * time.Millisecond)
		}
		emit(ui.SupTextDoneMsg{Text: text, Time: time.Now()})
	}

	emit(
		ui.SupInitMsg{SessionID: "35a53d5b-demo", Model: "claude-fable-5", Auth: "subscription", PID: 41772},
		ui.ModelStatMsg{Name: "qwen-coder", Note: "sglang · GX10"},
		ui.ModelStatMsg{Name: "qwen-small", Note: "sglang · single"},
		ui.SupRateLimitMsg{FiveHour: 0.04, SevenDay: 0.11},
		ui.RawLogMsg{Source: "app", Line: "demo replay — recorded streams, no live supervisor"},
	)
	sleep(800 * time.Millisecond)

	emit(ui.SupUserMsg{Text: "Build the trade query builder module. Use the spec in docs/trade-queries.md. Delegate the grunt work.", Time: time.Now()})
	emit(ui.SupStatusMsg{Status: "planning the split…"})
	sleep(1200 * time.Millisecond)
	emit(ui.SupStatusMsg{Status: ""})
	stream("Splitting this into four tasks: scaffold, rate-limit parsing, backoff, and the mod-filter builder. Delegating the first three; I'll review each result as it lands.")

	type deleg struct{ id, model, task string }
	first := []deleg{
		{"w1", "qwen-coder", "scaffold FastAPI app + healthz + job queue"},
		{"w2", "qwen-coder", "parse X-Rate-Limit headers into RateBudget"},
		{"w3", "qwen-coder", "implement rate-limit backoff in trade_client.py"},
	}
	for _, d := range first {
		emit(
			ui.SupToolMsg{ToolID: d.id, Name: "delegate", Delegate: &ui.DelegateInfo{Model: d.model, Task: d.task},
				Command: fmt.Sprintf("strawboss delegate --model %s --task %q", d.model, d.task)},
			ui.WorkerUpsertMsg{ID: d.id, Model: d.model, Task: d.task, Status: "running", Started: time.Now()},
		)
		sleep(400 * time.Millisecond)
	}
	emit(ui.ModelStatMsg{Name: "qwen-coder", TokSec: 74, Active: 3, Note: "sglang · GX10"})
	emit(ui.SupUsageMsg{Input: 1200, Output: 400, CacheRead: 14100, CacheWrite: 7200, CostUSD: 0.17, Turns: 1})
	emit(ui.SupStatusMsg{Status: "waiting on workers…"})

	// Live transcript for w3 from the real recorded opencode stream,
	// interleaved with usage ramps for the others.
	go rampUsage(ch, "w1", 9000, 63800, 16, speed)
	go rampUsage(ch, "w2", 7000, 41200, 12, speed)
	replayWorkerStream(ch, "w3", speed)

	emit(
		ui.WorkerUpsertMsg{ID: "w1", Status: "done", Summary: "scaffold up; 6/6 tests pass", LogPath: "~/.strawboss/logs/w1.jsonl"},
		ui.SupToolResultMsg{ToolID: "w1", Content: `w1 done 8:19 · log ~/.strawboss/logs/w1.jsonl — scaffold up; 6/6 tests pass`},
		ui.SupUsageMsg{Input: 40, Output: 90, CacheRead: 21000, CostUSD: 0.02, Turns: 1},
		ui.ModelStatMsg{Name: "qwen-coder", TokSec: 71, Active: 2, Note: "sglang · GX10"},
	)
	sleep(1500 * time.Millisecond)

	// w4: the failure — bell + toast.
	emit(
		ui.SupToolMsg{ToolID: "w4", Name: "delegate", Delegate: &ui.DelegateInfo{Model: "qwen-coder", Task: "mod-filter query builder + tests"},
			Command: `strawboss delegate --model qwen-coder --task "mod-filter query builder + tests"`},
		ui.WorkerUpsertMsg{ID: "w4", Model: "qwen-coder", Task: "mod-filter query builder + tests", Status: "running", Started: time.Now()},
		ui.ModelStatMsg{Name: "qwen-coder", TokSec: 74, Active: 3, Note: "sglang · GX10"},
	)
	go rampUsage(ch, "w4", 12000, 44000, 8, speed)
	sleep(5 * time.Second)
	emit(
		ui.WorkerUpsertMsg{ID: "w4", Status: "failed", Summary: "pytest: 3 failed in test_query_builder", LogPath: "~/.strawboss/logs/w4.jsonl"},
		ui.SupToolResultMsg{ToolID: "w4", Content: `w4 failed 3:12 · log ~/.strawboss/logs/w4.jsonl — pytest: 3 failed in test_query_builder`, IsError: true},
		ui.ModelStatMsg{Name: "qwen-coder", TokSec: 69, Active: 2, Note: "sglang · GX10"},
	)
	sleep(2 * time.Second)

	stream("w4's failures are all in mod-filter edge cases (pseudo-mods on jewelry). Dispatching w5 to fix them with the failing tests as the spec, and queueing a docstring pass.")
	emit(
		ui.SupToolMsg{ToolID: "w5", Name: "delegate", Delegate: &ui.DelegateInfo{Model: "qwen-coder", Task: "fix 3 failing tests in test_query_builder.py"}},
		ui.WorkerUpsertMsg{ID: "w5", Model: "qwen-coder", Task: "fix 3 failing tests in test_query_builder.py", Status: "running", Started: time.Now()},
		ui.WorkerUpsertMsg{ID: "w6", Model: "qwen-small", Task: "docstrings + type hints pass on trade module", Status: "queued"},
		ui.ModelStatMsg{Name: "qwen-coder", TokSec: 74, Active: 3, Note: "sglang · GX10"},
		ui.ModelStatMsg{Name: "qwen-small", Active: 0, Queue: 1, Note: "sglang · single"},
		ui.SupUsageMsg{Input: 60, Output: 210, CacheRead: 26000, CostUSD: 0.03, Turns: 1},
		ui.SupStatusMsg{Status: "reviewing w3's backoff diff before merging…"},
	)
	go rampUsage(ch, "w5", 3000, 22400, 10, speed)

	// The supervisor's own reply replayed from the real captured
	// stream-json fixture: deltas, tool call, tool result, final text.
	sleep(3 * time.Second)
	replaySupervisorStream(ch, speed)

	sleep(4 * time.Second)
	emit(
		ui.WorkerUpsertMsg{ID: "w2", Status: "done", Summary: "RateBudget parser + 9 tests green", LogPath: "~/.strawboss/logs/w2.jsonl"},
		ui.SupToolResultMsg{ToolID: "w2", Content: `w2 done 6:01 · log ~/.strawboss/logs/w2.jsonl — RateBudget parser + 9 tests green`},
		ui.SupUsageMsg{Input: 30, Output: 80, CacheRead: 24000, CostUSD: 0.02, Turns: 1},
		ui.SupStatusMsg{Status: ""},
	)
	stream("Scaffold and rate-limit parsing are merged. Backoff (w3) and the test fixes (w5) are still running; docstrings queued on qwen-small. I'll report when the failing tests are green.")
	emit(ui.SupRateLimitMsg{FiveHour: 0.05, SevenDay: 0.11})

	// Steady state: pulse the endpoint stats forever.
	for i := 0; ; i++ {
		sleep(2 * time.Second)
		tok := 68 + float64((i*7)%12)
		emit(ui.ModelStatMsg{Name: "qwen-coder", TokSec: tok, Active: 2, Note: "sglang · GX10"})
	}
}

// rampUsage ticks a worker's token counts up to target over steps.
func rampUsage(ch chan<- tea.Msg, id string, out, target, steps int, speed float64) {
	for i := 1; i <= steps; i++ {
		time.Sleep(time.Duration(float64(900*time.Millisecond) / speed))
		ch <- ui.WorkerUsageMsg{ID: id, Input: target * i / steps, Output: out * i / steps}
	}
}

// replayWorkerStream feeds the recorded opencode /global/event stream into
// worker id's transcript, exactly as the live harness mapping would.
func replayWorkerStream(ch chan<- tea.Msg, id string, speed float64) {
	sc := bufio.NewScanner(strings.NewReader(workerEvents))
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		ev, ok := opencode.ParseEventLine(sc.Text())
		if !ok {
			continue
		}
		switch ev.Type {
		case "message.part.delta":
			if ev.Properties.Field != "text" && ev.Properties.Field != "reasoning" {
				continue
			}
			ch <- ui.WorkerEventMsg{ID: id, Kind: ev.Properties.Field, Text: ev.Properties.Delta}
			time.Sleep(time.Duration(float64(120*time.Millisecond) / speed))
		case "message.part.updated":
			p := ev.Properties.Part
			if p == nil || p.Type != "tool" || (p.State.Status != "completed" && p.State.Status != "error") {
				continue
			}
			kind := "tool"
			if p.State.Status == "error" {
				kind = "error"
			}
			ch <- ui.WorkerEventMsg{ID: id, Kind: kind, Text: fmt.Sprintf("%s %s [%s]", p.Tool, p.State.Title, p.State.Status)}
			time.Sleep(time.Duration(float64(400*time.Millisecond) / speed))
		}
	}
	ch <- ui.WorkerUsageMsg{ID: id, Input: 16224, Output: 198}
}

// replaySupervisorStream replays the recorded claude stream-json turn
// through the real parser, mapped to UI msgs the way live mode will.
func replaySupervisorStream(ch chan<- tea.Msg, speed float64) {
	sc := bufio.NewScanner(strings.NewReader(string(supTurn)))
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		ev, _ := supervisor.ParseLine(sc.Bytes())
		if ev == nil {
			continue
		}
		switch e := ev.(type) {
		case supervisor.StreamDeltaEvent:
			ch <- ui.SupTextDeltaMsg{Text: e.Text}
			time.Sleep(time.Duration(float64(70*time.Millisecond) / speed))
		case supervisor.AssistantEvent:
			if e.Text != "" {
				ch <- ui.SupTextDoneMsg{Text: e.Text, Time: time.Now()}
			}
			for _, tu := range e.ToolUses {
				cmd, _ := tu.BashCommand()
				ch <- ui.SupToolMsg{ToolID: tu.ID, Name: tu.Name, Command: cmd}
			}
		case supervisor.ToolResultsEvent:
			for _, r := range e.Results {
				ch <- ui.SupToolResultMsg{ToolID: r.ToolUseID, Content: r.Content, IsError: r.IsError}
			}
		case supervisor.RateLimitEvent:
			ch <- ui.SupRateLimitMsg{FiveHour: e.FiveHour.Utilization, SevenDay: e.SevenDay.Utilization}
		case supervisor.ResultEvent:
			ch <- ui.SupUsageMsg{
				Input: e.Usage.InputTokens, Output: e.Usage.OutputTokens,
				CacheRead: e.Usage.CacheReadTokens, CacheWrite: e.Usage.CacheCreationTokens,
				CostUSD: e.TotalCostUSD, Turns: 1,
			}
		}
	}
}
