package ui

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// apply pushes msgs through Update and returns the evolved model.
func apply(t *testing.T, m Model, msgs ...tea.Msg) Model {
	t.Helper()
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		var ok bool
		m, ok = next.(Model)
		if !ok {
			t.Fatalf("Update returned %T", next)
		}
	}
	return m
}

func demoState(t *testing.T) Model {
	t.Helper()
	m := New(make(chan tea.Msg))
	m = apply(t, m,
		tea.WindowSizeMsg{Width: 120, Height: 40},
		SupInitMsg{SessionID: "0123456789abcdef", Model: "claude-fable-5", Auth: "subscription", PID: 41772},
		ModelStatMsg{Name: "qwen-coder", TokSec: 74, Active: 2, Note: "sglang · GX10"},
		ModelStatMsg{Name: "qwen-small", Note: "sglang · single"},
		SupRateLimitMsg{FiveHour: 0.04, SevenDay: 0.11},
		SupUserMsg{Text: "Build the trade query builder module.", Time: time.Now()},
		SupTextDoneMsg{Text: "Splitting this into four tasks.", Time: time.Now()},
		SupToolMsg{ToolID: "t1", Name: "delegate", Delegate: &DelegateInfo{Model: "qwen-coder", Task: "scaffold the app"}},
		WorkerUpsertMsg{ID: "w1", Model: "qwen-coder", Task: "scaffold the app", Status: "running", Started: time.Now()},
		WorkerUsageMsg{ID: "w1", Input: 30000, Output: 8200},
		WorkerEventMsg{ID: "w1", Kind: "tool", Text: "bash pytest -q [completed]"},
		WorkerUpsertMsg{ID: "w2", Model: "qwen-small", Task: "docstrings", Status: "queued"},
		SupUsageMsg{Input: 1200, Output: 400, CacheRead: 14100, CacheWrite: 7200, CostUSD: 0.17, Turns: 1},
	)
	return m
}

func TestWorkerLifecycleAndBell(t *testing.T) {
	m := demoState(t)
	if len(m.workers) != 2 {
		t.Fatalf("workers = %d", len(m.workers))
	}
	if m.workers[0].Status != "running" || m.workers[0].In != 30000 {
		t.Errorf("w1 = %+v", m.workers[0])
	}
	if m.toast != "" {
		t.Errorf("toast before failure: %q", m.toast)
	}

	m = apply(t, m, WorkerUpsertMsg{ID: "w1", Status: "failed", Summary: "pytest: 3 failed"})
	if m.workers[0].Status != "failed" {
		t.Errorf("w1 = %+v", m.workers[0])
	}
	if !strings.Contains(m.toast, "w1 failed") {
		t.Errorf("toast = %q", m.toast)
	}
	if m.workers[0].Ended.IsZero() {
		t.Error("Ended not set")
	}
}

func TestStreamingFlushAndFinalize(t *testing.T) {
	m := New(make(chan tea.Msg))
	m = apply(t, m, SupTextDeltaMsg{Text: "partial "}, SupTextDeltaMsg{Text: "text"})
	if m.streaming.String() != "partial text" {
		t.Errorf("streaming = %q", m.streaming.String())
	}
	// The final message replaces the accumulated deltas — no duplicate.
	m = apply(t, m, SupTextDoneMsg{Text: "partial text plus tail", Time: time.Now()})
	if m.streaming.Len() != 0 {
		t.Error("streaming not reset")
	}
	if len(m.chat) != 1 || m.chat[0].text != "partial text plus tail" {
		t.Errorf("chat = %+v", m.chat)
	}
}

func TestUsageAccumulates(t *testing.T) {
	m := New(make(chan tea.Msg))
	m = apply(t, m,
		SupUsageMsg{Input: 10, Output: 5, CacheRead: 100, CostUSD: 0.01, Turns: 1},
		SupUsageMsg{Input: 20, Output: 15, CacheRead: 200, CostUSD: 0.02, Turns: 1},
	)
	if m.supIn != 30 || m.supOut != 20 || m.supCacheRead != 300 || m.supTurns != 2 {
		t.Errorf("usage = in=%d out=%d cache=%d turns=%d", m.supIn, m.supOut, m.supCacheRead, m.supTurns)
	}
}

func TestTabKeys(t *testing.T) {
	m := demoState(t)
	if m.tab != tabChat {
		t.Fatalf("start tab = %d", m.tab)
	}
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if m.tab != tabDashboard {
		t.Errorf("tab = %d, want dashboard", m.tab)
	}
	// digits work outside the chat tab
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3")})
	if m.tab != tabLogs {
		t.Errorf("tab = %d, want logs", m.tab)
	}
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	if m.tab != tabChat {
		t.Errorf("tab = %d, want chat", m.tab)
	}
	// in chat, digits type into the input instead
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if m.tab != tabChat || m.input.Value() != "2" {
		t.Errorf("tab = %d input = %q", m.tab, m.input.Value())
	}
}

func TestEnterEmitsPromptAndDemoNote(t *testing.T) {
	m := demoState(t)
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi there")})
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.input.Value() != "" {
		t.Errorf("input not cleared: %q", m.input.Value())
	}
	if m.chat[len(m.chat)-1].kind != "user" || m.chat[len(m.chat)-1].text != "hi there" {
		t.Errorf("chat tail = %+v", m.chat[len(m.chat)-1])
	}
	if cmd == nil {
		t.Fatal("no cmd")
	}
	msg := cmd()
	sp, ok := msg.(SendPromptMsg)
	if !ok || sp.Text != "hi there" {
		t.Fatalf("cmd msg = %#v", msg)
	}
	// Without a live handler, the prompt lands as a demo note.
	m = apply(t, m, sp)
	if m.chat[len(m.chat)-1].kind != "note" {
		t.Errorf("chat tail = %+v", m.chat[len(m.chat)-1])
	}
}

func TestPromptHandlerWins(t *testing.T) {
	m := demoState(t)
	var got string
	m.OnPrompt = func(text string) { got = text }
	m = apply(t, m, SendPromptMsg{Text: "do a thing"})
	if got != "do a thing" {
		t.Errorf("handler got %q", got)
	}
	if m.chat[len(m.chat)-1].kind == "note" {
		t.Error("demo note should not appear with a handler")
	}
}

func TestSortedWorkers(t *testing.T) {
	m := New(make(chan tea.Msg))
	m = apply(t, m,
		WorkerUpsertMsg{ID: "w1", Status: "done"},
		WorkerUpsertMsg{ID: "w2", Status: "running"},
		WorkerUpsertMsg{ID: "w3", Status: "queued"},
	)
	rows := m.sortedWorkers()
	if rows[0].ID != "w3" || rows[1].ID != "w2" || rows[2].ID != "w1" {
		t.Errorf("order = %s %s %s", rows[0].ID, rows[1].ID, rows[2].ID)
	}
}

func TestViewsRenderKeyContent(t *testing.T) {
	m := demoState(t)

	chat := m.View()
	for _, want := range []string{"strawboss", "chat", "dashboard", "logs",
		"TOKENS", "WORKERS · 1 ACTIVE", "MODELS", "qwen-coder", "74 tok/s",
		"plan", "$0.00", "bell on fail", "YOU ·", "SUPERVISOR ·",
		"Build the trade query builder module.", "w1", "5h 4% · 7d 11%"} {
		if !strings.Contains(chat, want) {
			t.Errorf("chat view missing %q", want)
		}
	}

	m.tab = tabDashboard
	dash := m.View()
	for _, want := range []string{"SUPERVISOR · PLAN", "WORKERS · LOCAL", "TASKS",
		"scaffold the app", "running", "queued", "SUPERVISOR DETAIL",
		"subscription", "marginal cost", "cache", "WORKER W2 · QWEN-SMALL"} {
		if !strings.Contains(dash, want) {
			t.Errorf("dashboard view missing %q", want)
		}
	}

	m.tab = tabLogs
	logs := m.View()
	for _, want := range []string{"LOGS", "w1 running"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs view missing %q", want)
		}
	}

	if dump := os.Getenv("STRAWBOSS_DUMP"); dump != "" {
		m.tab = tabChat
		out := m.View() + "\n\n════════ DASHBOARD ════════\n\n"
		m.tab = tabDashboard
		out += m.View()
		os.WriteFile(dump, []byte(out), 0o644)
	}
}

// TestWorkerStreamScrolls: streamed deltas must flush completed lines into
// the transcript (scrolling), never accumulate into one rewriting line.
func TestWorkerStreamScrolls(t *testing.T) {
	m := New(make(chan tea.Msg))
	m = apply(t, m, WorkerUpsertMsg{ID: "w1", Model: "qwen-coder", Task: "t", Status: "running"})

	// Prose with newlines arrives in small deltas.
	for _, d := range []string{"First line", " of text\nSecond", " line\n\nThird grows"} {
		m = apply(t, m, WorkerEventMsg{ID: "w1", Kind: "reasoning", Text: d})
	}
	evs := m.workerEvents["w1"]
	if len(evs) != 3 {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].text != "First line of text" || !evs[0].done {
		t.Errorf("evs[0] = %+v", evs[0])
	}
	if evs[1].text != "Second line" || !evs[1].done {
		t.Errorf("evs[1] = %+v", evs[1])
	}
	if evs[2].text != "Third grows" || evs[2].done {
		t.Errorf("evs[2] = %+v", evs[2])
	}

	// A long unbroken paragraph force-wraps instead of growing forever.
	m = apply(t, m, WorkerEventMsg{ID: "w1", Kind: "reasoning", Text: strings.Repeat("word ", 80)})
	evs = m.workerEvents["w1"]
	for _, ev := range evs[:len(evs)-1] {
		if len(ev.text) > flushWidth {
			t.Errorf("flushed line too long (%d): %q…", len(ev.text), ev.text[:40])
		}
	}
	if n := len(evs); n < 5 {
		t.Errorf("long paragraph did not wrap into lines: %d entries", n)
	}

	// A tool event closes the growing line and logs to the logs tab.
	before := len(m.logs)
	m = apply(t, m, WorkerEventMsg{ID: "w1", Kind: "tool", Text: "bash pytest [completed]"})
	evs = m.workerEvents["w1"]
	if !evs[len(evs)-1].done || evs[len(evs)-1].kind != "tool" {
		t.Errorf("tail = %+v", evs[len(evs)-1])
	}
	if !evs[len(evs)-2].done {
		t.Errorf("growing line not closed by tool event: %+v", evs[len(evs)-2])
	}
	if len(m.logs) != before+1 {
		t.Errorf("tool event not logged (logs %d → %d)", before, len(m.logs))
	}
}

// TestToolLinesWrap: denial reasons and task text wrap across lines in the
// chat instead of disappearing behind an ellipsis.
func TestToolLinesWrap(t *testing.T) {
	m := New(make(chan tea.Msg))
	long := "Permission to use Write has been denied because Claude Code is running in don't ask mode. IMPORTANT: You may attempt to accomplish this action using other tools."
	m = apply(t, m,
		tea.WindowSizeMsg{Width: 100, Height: 40},
		SupToolResultMsg{ToolID: "t1", Content: long, IsError: true},
	)
	view := m.View()
	// Single tokens (wrapping may break phrases across lines) from the
	// middle and the very end of the message.
	for _, want := range []string{"IMPORTANT:", "accomplish", "tools."} {
		if !strings.Contains(view, want) {
			t.Errorf("chat view lost %q", want)
		}
	}
}

func TestWorkerKillRetryKeys(t *testing.T) {
	m := demoState(t) // w1 running, w2 queued → display order: w2, w1
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if m.tab != tabDashboard {
		t.Fatalf("tab = %d", m.tab)
	}
	var killed, retried string
	m.OnKillWorker = func(id string) { killed = id }
	m.OnRetryWorker = func(id string) { retried = id }

	// Retry on the selected queued worker is refused with a toast.
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if !strings.Contains(m.toast, "still running") {
		t.Errorf("toast = %q", m.toast)
	}

	// Kill emits KillWorkerMsg for the selected worker and reaches the hook.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("x produced no cmd")
	}
	km, ok := cmd().(KillWorkerMsg)
	if !ok || km.ID != "w2" {
		t.Fatalf("cmd msg = %#v", km)
	}
	m = apply(t, m, km)
	if killed != "w2" {
		t.Errorf("killed = %q", killed)
	}

	// Once w2 fails, kill refuses and retry goes through.
	m = apply(t, m, WorkerUpsertMsg{ID: "w2", Status: "failed", Summary: "aborted"})
	m.selected = 1 // display order now: w1 (running), w2 (failed)
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if !strings.Contains(m.toast, "nothing to kill") {
		t.Errorf("toast = %q", m.toast)
	}
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("r produced no cmd")
	}
	rm, ok := cmd().(RetryWorkerMsg)
	if !ok || rm.ID != "w2" {
		t.Fatalf("cmd msg = %#v", rm)
	}
	m = apply(t, m, rm)
	if retried != "w2" {
		t.Errorf("retried = %q", retried)
	}
}

func TestToastMsgShowsAndExpires(t *testing.T) {
	m := New(make(chan tea.Msg))
	m = apply(t, m, ToastMsg{Text: "retrying w3's task on qwen-coder"})
	if !strings.Contains(m.toast, "retrying w3") {
		t.Errorf("toast = %q", m.toast)
	}
	m = apply(t, m, tickMsg(time.Now().Add(10*time.Second)))
	if m.toast != "" {
		t.Errorf("toast survived expiry: %q", m.toast)
	}
}

func TestWorkerFilter(t *testing.T) {
	m := demoState(t)
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyTab}) // dashboard
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if !m.filtering {
		t.Fatal("/ did not open the filter")
	}
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w2")},
		tea.KeyMsg{Type: tea.KeyEnter})
	if m.filtering || m.filter != "w2" {
		t.Fatalf("filtering=%v filter=%q", m.filtering, m.filter)
	}
	rows := m.visibleWorkers()
	if len(rows) != 1 || rows[0].ID != "w2" {
		t.Fatalf("rows = %+v", rows)
	}
	// "!" shorthand = running.
	m.filter = "!"
	rows = m.visibleWorkers()
	if len(rows) != 1 || rows[0].Status != "running" {
		t.Fatalf("running rows = %+v", rows)
	}
	// esc clears an applied filter before leaving the tab.
	m.filter = "w2"
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.filter != "" || m.tab != tabDashboard {
		t.Fatalf("filter=%q tab=%d", m.filter, m.tab)
	}
}

func TestLogsSourceFilterCycles(t *testing.T) {
	m := demoState(t)
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3")})
	t.Log("won't reach logs from chat; go via tab")
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyTab}, tea.KeyMsg{Type: tea.KeyTab}) // chat→dash→logs
	if m.tab != tabLogs {
		t.Fatalf("tab = %d", m.tab)
	}
	want := []string{"sup", "wrk", "app", ""}
	for _, exp := range want {
		m = apply(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
		if m.logSrc != exp {
			t.Fatalf("logSrc = %q, want %q", m.logSrc, exp)
		}
	}
}

func TestSidePanelEconomyAndModels(t *testing.T) {
	m := New(make(chan tea.Msg))
	m = apply(t, m,
		tea.WindowSizeMsg{Width: 120, Height: 40},
		// Cache-heavy supervisor: 300k cache reads vs 5k fresh — the bar
		// must reflect fresh spend, not raw volume.
		SupUsageMsg{Input: 3000, Output: 2000, CacheRead: 300000, Turns: 1},
		WorkerUpsertMsg{ID: "w1", Model: "qwen-dsh", Task: "t", Status: "done"},
		WorkerUsageMsg{ID: "w1", Input: 40000, Output: 5000},
		ModelStatMsg{Name: "deepseek-dsh", Note: "model not loaded"},
		ModelStatMsg{Name: "qwen-coder", Note: "opencode"},
		ModelStatMsg{Name: "qwen-dsh", Note: "dsh"},
	)
	tokens := m.viewTokensPanel(38)
	if !strings.Contains(tokens, "fresh") {
		t.Errorf("tokens panel missing fresh legend:\n%s", tokens)
	}
	// fresh sup = 5k vs workers 45k → plan share must be small (10%).
	if !strings.Contains(tokens, "10%") {
		t.Errorf("fresh split not computed over fresh tokens:\n%s", tokens)
	}

	models := m.viewModelsPanel(38)
	if !strings.Contains(models, "not loaded") || !strings.Contains(models, "deepseek-dsh") {
		t.Errorf("not-loaded model hidden:\n%s", models)
	}
	if !strings.Contains(models, "+2 idle") {
		t.Errorf("idle models not collapsed:\n%s", models)
	}
	if strings.Contains(models, "qwen-coder") {
		t.Errorf("healthy idle model got its own row:\n%s", models)
	}

	// All idle → a single count line.
	m = apply(t, m, ModelStatMsg{Name: "deepseek-dsh", Note: "dsh"})
	models = m.viewModelsPanel(38)
	if !strings.Contains(models, "3 configured · all idle") {
		t.Errorf("all-idle collapse missing:\n%s", models)
	}
}

func TestRetryAllFailed(t *testing.T) {
	m := demoState(t)
	m = apply(t, m,
		WorkerUpsertMsg{ID: "w1", Status: "failed", Summary: "boom"},
		WorkerUpsertMsg{ID: "w3", Model: "qwen-dsh", Task: "t3", Status: "failed", Summary: "bang"},
		tea.KeyMsg{Type: tea.KeyTab})
	var retried []string
	m.OnRetryWorker = func(id string) { retried = append(retried, id) }

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("R produced no cmd")
	}
	// Batch cmds resolve to RetryWorkerMsg per failed worker; feed them back.
	if msg := cmd(); msg != nil {
		collect := func(ms tea.Msg) {
			if rm, ok := ms.(RetryWorkerMsg); ok {
				m = apply(t, m, rm)
			}
			if batch, ok := ms.(tea.BatchMsg); ok {
				for _, c := range batch {
					if inner := c(); inner != nil {
						if rm, ok := inner.(RetryWorkerMsg); ok {
							m = apply(t, m, rm)
						}
					}
				}
			}
		}
		collect(msg)
	}
	if len(retried) != 2 {
		t.Fatalf("retried = %v, want w1 and w3", retried)
	}
	// A filter scopes the sweep.
	m.filter = "bang"
	retried = nil
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("R")})
	m = next.(Model)
	if !strings.Contains(m.toast, "retrying 1 failed") {
		t.Errorf("toast = %q", m.toast)
	}
}
