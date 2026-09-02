package ui

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	// The headline is fresh tokens (5k), never cache-inflated (305k),
	// with cache reads on their own dim line and a cost-weighted split.
	if !strings.Contains(tokens, "5.0k") || !strings.Contains(tokens, "cache reads") {
		t.Errorf("headline not fresh:\n%s", tokens)
	}
	if strings.Contains(tokens, "305.0k") {
		t.Errorf("cache-inflated headline:\n%s", tokens)
	}
	if !strings.Contains(tokens, "plan-equiv") || !strings.Contains(tokens, "leverage") {
		t.Errorf("cost-weighted split missing:\n%s", tokens)
	}

	models := m.viewModelsPanel(38)
	if !strings.Contains(models, "not loaded") || !strings.Contains(models, "deepseek-dsh") {
		t.Errorf("not-loaded model hidden:\n%s", models)
	}
	if !strings.Contains(models, "qwen-coder, qwen-dsh") {
		t.Errorf("idle models not named:\n%s", models)
	}

	// All idle → one line naming them (or a count when they don't fit).
	m = apply(t, m, ModelStatMsg{Name: "deepseek-dsh", Note: "dsh"})
	models = m.viewModelsPanel(38)
	if !strings.Contains(models, "3 models") && !strings.Contains(models, "deepseek-dsh") {
		t.Errorf("all-idle line missing:\n%s", models)
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

func TestWorkerDetailShowsThroughputAndContext(t *testing.T) {
	m := demoState(t)
	m = apply(t, m,
		ModelStatMsg{Name: "qwen-coder", Note: "opencode", ContextWindow: 262144},
		WorkerUsageMsg{ID: "w1", Input: 40000, Output: 9000, Ctx: 30000},
	)
	m.selected = 1 // display order: w2 (queued), w1 (running)
	out := m.viewDetailSplit(120, 20)
	for _, want := range []string{"ctx 30.0k", "262.1k", "avg", "started", "log "} {
		if !strings.Contains(out, want) {
			t.Errorf("detail missing %q:\n%s", want, out)
		}
	}
}

func TestDetailExtrasAndRecentResults(t *testing.T) {
	m := demoState(t)
	// Three usage updates with growing output → rate history + steps.
	for i, out := range []int{1000, 3000, 6000} {
		r := m.workerRates["w1"]
		r.at = time.Now().Add(-time.Second) // age the sample so a delta computes
		m.workerRates["w1"] = r
		m = apply(t, m, WorkerUsageMsg{ID: "w1", Input: 10000 + i, Output: out, Ctx: 5000 + i})
	}
	if m.worker("w1").Steps != 3 {
		t.Errorf("steps = %d", m.worker("w1").Steps)
	}
	if len(m.workerRates["w1"].hist) < 2 {
		t.Fatalf("hist = %v", m.workerRates["w1"].hist)
	}

	// Failed worker surfaces its last transcript error above the summary.
	m = apply(t, m,
		WorkerEventMsg{ID: "w1", Kind: "error", Text: "bash: tests exploded"},
		WorkerUpsertMsg{ID: "w1", Status: "failed", Summary: "worker stopped"},
		SupToolResultMsg{ToolID: "t1", Content: "w1 failed 2m10s · log /l\nworker stopped", IsError: true},
		SupToolResultMsg{ToolID: "t2", Content: "some Read output that is not a delegation"},
	)
	m.selected = 0 // failed w1 sorts after queued w2… find it
	rows := m.visibleWorkers()
	for i, r := range rows {
		if r.ID == "w1" {
			m.selected = i
		}
	}
	out := m.viewDetailSplit(140, 24)
	for _, want := range []string{"tests exploded", "summary: worker stopped", "step 3",
		"recent delegation results", "w1 failed 2m10s"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail missing %q", want)
		}
	}
	if strings.Contains(out, "not a delegation") {
		t.Error("non-delegation tool result leaked into recent results")
	}
	if len(m.workerRates["w1"].hist) >= 3 {
		spark := sparkline(m.workerRates["w1"].hist, 20)
		if spark == "" {
			t.Error("sparkline empty despite history")
		}
	}
}

func TestNormalizeDroppedPaths(t *testing.T) {
	cases := map[string]string{
		`look at "C:\Users\josh\my file.txt" please`: "look at /mnt/c/Users/josh/my file.txt please",
		`check C:\temp\x.log`:                        "check /mnt/c/temp/x.log",
		"plain /home/josh/file.go untouched":         "plain /home/josh/file.go untouched",
		`D:\data\set.csv and E:\other\y.txt`:         "/mnt/d/data/set.csv and /mnt/e/other/y.txt",
	}
	for in, want := range cases {
		if got := normalizeDroppedPaths(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionPicker(t *testing.T) {
	m := demoState(t)
	var switched []string
	m.OnListSessions = func() []SessionInfo {
		return []SessionInfo{
			{ID: "ses-new", Run: "run-2", Label: "current work", Current: true, Workers: 3, Done: 2},
			{ID: "ses-old", Run: "run-1", Label: "farkle game", Workers: 12, Done: 9, Failed: 3},
		}
	}
	m.OnSwitchSession = func(id, run string) { switched = append(switched, id+"/"+run) }

	m = apply(t, m, tea.KeyMsg{Type: tea.KeyTab}) // dashboard
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if !m.picking || len(m.sessions) != 2 {
		t.Fatalf("picking=%v sessions=%d", m.picking, len(m.sessions))
	}
	out := m.viewSessionPicker(120, 20)
	for _, want := range []string{"farkle game", "current work", "current", "12w"} {
		if !strings.Contains(out, want) {
			t.Errorf("picker missing %q", want)
		}
	}
	// Enter on the current session: no switch.
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(switched) != 0 || m.picking {
		t.Fatalf("switched=%v picking=%v", switched, m.picking)
	}
	// Reopen, pick the old one.
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")},
		tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyEnter})
	if len(switched) != 1 || switched[0] != "ses-old/run-1" {
		t.Fatalf("switched = %v", switched)
	}
	// The switch announcement resets chat and workers.
	m = apply(t, m, SessionSwitchedMsg{ID: "ses-old"})
	if len(m.workers) != 0 || m.sessionID != "ses-old" {
		t.Errorf("workers=%d session=%q", len(m.workers), m.sessionID)
	}
	if len(m.chat) != 1 || m.chat[0].kind != "note" {
		t.Errorf("chat = %+v", m.chat)
	}
}

func TestLoudDenials(t *testing.T) {
	m := demoState(t)
	denial := "Permission to use Bash has been denied because Claude Code is running in don't ask mode. IMPORTANT: ..."
	m = apply(t, m,
		SupToolMsg{ToolID: "t9", Name: "Bash", Command: "ls -la /home/josh/git/test_job/"},
		SupToolResultMsg{ToolID: "t9", Content: denial, IsError: true},
	)
	last := m.chat[len(m.chat)-2] // note lands before the tool-in? order: tool-out, tool-in, note appended after
	found := false
	for _, it := range m.chat {
		if it.kind == "note" && strings.Contains(it.text, `"Bash(ls:*)"`) &&
			strings.Contains(it.text, "allowed_tools") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no loud denial note; last chat = %+v", last)
	}
	if !strings.Contains(m.toast, "denied Bash") {
		t.Errorf("toast = %q", m.toast)
	}
	// The same suggestion never repeats.
	notes := 0
	m = apply(t, m,
		SupToolMsg{ToolID: "t10", Name: "Bash", Command: "ls /tmp"},
		SupToolResultMsg{ToolID: "t10", Content: denial, IsError: true},
	)
	for _, it := range m.chat {
		if it.kind == "note" && strings.Contains(it.text, "Bash(ls:*)") {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("suggestion repeated %d times", notes)
	}
	// A non-Bash denial suggests the bare tool.
	if got := AllowSuggestion("WebSearch", ""); got != "WebSearch" {
		t.Errorf("suggestion = %q", got)
	}
}

func TestChatRenderingTamed(t *testing.T) {
	// Unbroken runs must never exceed the column: a JSON blob in a tool
	// result once shoved the side panel off-screen.
	long := strings.Repeat(`{"type":"assistant/chunk","seq":1,`, 30)
	m := demoState(t)
	m = apply(t, m,
		SupToolMsg{ToolID: "tr", Name: "Read", Command: "Read /some/log.jsonl"},
		SupToolResultMsg{ToolID: "tr", Content: long},
		SupTextDoneMsg{Text: "All **four workers** finished; run `advisor.py report` next.", Time: time.Now()},
	)
	out := m.viewChat(120, 40)
	for _, line := range strings.Split(out, "\n") {
		if lipgloss.Width(line) > 120 {
			t.Fatalf("line overflows column (%d cols): %.80q", lipgloss.Width(line), line)
		}
	}
	// Non-delegation tool result collapsed hard (160 cap + ellipsis).
	if strings.Count(out, `"seq":1`) > 8 {
		t.Error("raw tool result not collapsed")
	}
	// Markdown styled, not asterisk soup.
	if strings.Contains(out, "**four workers**") {
		t.Error("bold markers rendered literally")
	}
	if !strings.Contains(out, "four workers") || !strings.Contains(out, "advisor.py report") {
		t.Error("styled text lost content")
	}
}

func TestMdInlineUnpaired(t *testing.T) {
	if got := mdInline("a ** b"); !strings.Contains(got, "** b") {
		t.Errorf("unpaired marker mangled: %q", got)
	}
	if got := mdInline("x **bold** y `code` z"); strings.Contains(got, "**") || strings.Contains(got, "`") {
		t.Errorf("markers left in: %q", got)
	}
}

func TestFeedBatchFoldsInOneUpdate(t *testing.T) {
	m := New(make(chan tea.Msg))
	batch := feedBatch{
		WorkerUpsertMsg{ID: "w1", Model: "m", Task: "a", Status: "running"},
		WorkerUsageMsg{ID: "w1", Input: 100, Output: 50},
		WorkerUpsertMsg{ID: "w2", Model: "m", Task: "b", Status: "done"},
		SupUsageMsg{Input: 10, Output: 5, Turns: 1},
	}
	next, cmd := m.Update(batch)
	m = next.(Model)
	if len(m.workers) != 2 || m.workers[0].Out != 50 || m.supTurns != 1 {
		t.Fatalf("batch not folded: workers=%d", len(m.workers))
	}
	if cmd == nil {
		t.Fatal("batch did not re-arm the listener")
	}
}

// TestLiveTurnUsageShowsMidTurn: the supervisor counter must move DURING
// a long turn (per-call estimates), then snap to the authoritative turn
// totals without double-counting.
func TestLiveTurnUsageShowsMidTurn(t *testing.T) {
	m := New(make(chan tea.Msg))
	m = apply(t, m,
		tea.WindowSizeMsg{Width: 120, Height: 40},
		SupTurnUsageMsg{Input: 100, Output: 50, CacheRead: 2000, CacheWrite: 500},
		SupTurnUsageMsg{Input: 30, Output: 20, CacheRead: 2500},
	)
	in, cacheR, cacheW, out := m.supTokens()
	if in != 130 || out != 70 || cacheR != 4500 || cacheW != 500 {
		t.Fatalf("mid-turn totals = %d/%d/%d/%d", in, cacheR, cacheW, out)
	}
	tokens := m.viewTokensPanel(38)
	if !strings.Contains(tokens, "700 · ") {
		t.Errorf("mid-turn fresh total (700) not shown:\n%s", tokens)
	}

	// The turn result is authoritative: commit and drop the estimate.
	m = apply(t, m, SupUsageMsg{Input: 140, Output: 75, CacheRead: 4600, CacheWrite: 500, Turns: 1})
	in, cacheR, _, out = m.supTokens()
	if in != 140 || out != 75 || cacheR != 4600 {
		t.Fatalf("post-turn totals double-counted: %d/%d/%d", in, cacheR, out)
	}

	// An interrupted turn keeps its estimates.
	m = apply(t, m, SupTurnUsageMsg{Input: 10, Output: 5}, SupTurnDoneMsg{Interrupted: true})
	in, _, _, out = m.supTokens()
	if in != 150 || out != 80 {
		t.Fatalf("interrupted-turn estimate lost: %d/%d", in, out)
	}
}

func TestTokensPanelLeverage(t *testing.T) {
	m := New(make(chan tea.Msg))
	m = apply(t, m,
		tea.WindowSizeMsg{Width: 120, Height: 40},
		// Supervisor: 1.0M fresh + 30M cache reads → plan-equiv 4.0M.
		SupUsageMsg{Input: 600000, Output: 400000, CacheRead: 30000000, Turns: 1},
		// Workers: 16M fresh + 3M cache reads.
		WorkerUpsertMsg{ID: "w1", Model: "qwen-dsh", Task: "t", Status: "done"},
		WorkerUsageMsg{ID: "w1", Input: 6000000, CacheRead: 3000000, Output: 10000000},
	)
	tokens := m.viewTokensPanel(40)
	for _, want := range []string{"plan-equiv", "leverage", "≈4.0x", "1:1 assumption", "~10% rate", "free"} {
		if !strings.Contains(tokens, want) {
			t.Errorf("panel missing %q:\n%s", want, tokens)
		}
	}
	// plan-equiv share: 4M / 20M = 20%.
	if !strings.Contains(tokens, "20%") || !strings.Contains(tokens, "80%") {
		t.Errorf("cost-weighted split wrong:\n%s", tokens)
	}
}

// TestSupContextTracksAndWarns: the context footprint follows each API
// call's full prompt size, the fresh-session advice fires exactly once
// past the warning line, and both token panels surface the number.
func TestSupContextTracksAndWarns(t *testing.T) {
	m := demoState(t)
	m = apply(t, m, SupTurnUsageMsg{Input: 400, CacheRead: 30_000, CacheWrite: 2_000})
	if m.supCtx != 32_400 || m.ctxWarned {
		t.Fatalf("supCtx=%d warned=%v", m.supCtx, m.ctxWarned)
	}
	chatLen := len(m.chat)

	m = apply(t, m, SupTurnUsageMsg{Input: 500, CacheRead: 480_000})
	if m.supCtx != 480_500 || !m.ctxWarned {
		t.Fatalf("supCtx=%d warned=%v", m.supCtx, m.ctxWarned)
	}
	if len(m.chat) != chatLen+1 || !strings.Contains(m.chat[len(m.chat)-1].text, "/new") {
		t.Fatalf("expected one /new advice note, chat tail = %+v", m.chat[len(m.chat)-1])
	}
	// Crossing again stays quiet — advice is once per session.
	m = apply(t, m, SupTurnUsageMsg{Input: 500, CacheRead: 490_000})
	if len(m.chat) != chatLen+1 {
		t.Errorf("advice repeated: chat len %d", len(m.chat))
	}

	for name, out := range map[string]string{
		"chat panel": m.viewTokensPanel(46),
		"dashboard":  m.viewDashboard(120, 38),
	} {
		if !strings.Contains(out, "context") || !strings.Contains(out, "490.5k") {
			t.Errorf("%s missing context footprint", name)
		}
	}

	// A turn-total usage msg without ctx keeps the last known value.
	m = apply(t, m, SupUsageMsg{Input: 9000, CacheRead: 900_000, Turns: 1})
	if m.supCtx != 490_500 {
		t.Errorf("turn totals clobbered ctx: %d", m.supCtx)
	}
}

// TestSupContextSeededOnResume: the persisted ledger's ctx (SupUsageMsg
// seed) surfaces a bloated resumed session BEFORE the first prompt burns.
func TestSupContextSeededOnResume(t *testing.T) {
	m := New(make(chan tea.Msg))
	m = apply(t, m, tea.WindowSizeMsg{Width: 120, Height: 40},
		SupUsageMsg{Input: 8000, CacheRead: 2_400_000, Turns: 12, Ctx: 512_000})
	if m.supCtx != 512_000 || !m.ctxWarned {
		t.Fatalf("supCtx=%d warned=%v", m.supCtx, m.ctxWarned)
	}
	if len(m.chat) == 0 || !strings.Contains(m.chat[len(m.chat)-1].text, "/new") {
		t.Errorf("no resume advice in chat: %+v", m.chat)
	}
}

// TestNewSessionCommandAndKeys: `/new` in chat, `n` on the dashboard and
// in the picker all reach OnNewSession; the fresh-session announcement
// resets the context footprint and re-arms the advice.
func TestNewSessionCommandAndKeys(t *testing.T) {
	m := demoState(t)
	fresh := 0
	m.OnNewSession = func() { fresh++ }
	m = apply(t, m, SupTurnUsageMsg{CacheRead: 200_000}) // warned state

	// /new typed in chat: emits NewSessionMsg, sends no prompt.
	m.input.SetValue("/new")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("/new produced no cmd")
	}
	nm, ok := cmd().(NewSessionMsg)
	if !ok {
		t.Fatalf("cmd msg = %#v", nm)
	}
	m = apply(t, m, nm)
	if fresh != 1 {
		t.Fatalf("OnNewSession calls = %d", fresh)
	}

	// n on the dashboard.
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyTab})
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("dashboard n produced no cmd")
	}
	m = apply(t, m, cmd())
	if fresh != 2 {
		t.Fatalf("OnNewSession calls = %d", fresh)
	}

	// n inside the session picker closes it and asks for a fresh session.
	m.OnListSessions = func() []SessionInfo { return []SessionInfo{{ID: "ses-1"}} }
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = next.(Model)
	if m.picking || cmd == nil {
		t.Fatalf("picking=%v cmd=%v", m.picking, cmd)
	}
	m = apply(t, m, cmd())
	if fresh != 3 {
		t.Fatalf("OnNewSession calls = %d", fresh)
	}

	// The announcement (empty ID) resets ctx state and notes the fresh start.
	m = apply(t, m, SessionSwitchedMsg{})
	if m.supCtx != 0 || m.ctxWarned || m.sessionID != "" {
		t.Errorf("supCtx=%d warned=%v session=%q", m.supCtx, m.ctxWarned, m.sessionID)
	}
	if len(m.chat) != 1 || !strings.Contains(m.chat[0].text, "fresh session") {
		t.Errorf("chat = %+v", m.chat)
	}
}

// TestChatScroll: PgUp walks back into a long reply, an indicator says
// how to get back, PgDn/send re-join the live tail.
func TestChatScroll(t *testing.T) {
	m := New(make(chan tea.Msg))
	m = apply(t, m, tea.WindowSizeMsg{Width: 120, Height: 20})
	for i := 0; i < 60; i++ {
		m = apply(t, m, RawLogMsg{Source: "app", Line: "x"}) // unrelated
		m.chat = append(m.chat, chatItem{kind: "note", when: time.Now(), text: fmt.Sprintf("line-%02d", i)})
	}

	bottom := m.viewChatColumn(80, 18)
	if !strings.Contains(bottom, "line-59") || strings.Contains(bottom, "line-05") {
		t.Fatalf("unscrolled view wrong:\n%s", bottom)
	}

	m = apply(t, m, tea.KeyMsg{Type: tea.KeyPgUp}, tea.KeyMsg{Type: tea.KeyPgUp},
		tea.KeyMsg{Type: tea.KeyPgUp}, tea.KeyMsg{Type: tea.KeyPgUp})
	if m.chatScroll == 0 {
		t.Fatal("pgup did not scroll")
	}
	scrolled := m.viewChatColumn(80, 18)
	if !strings.Contains(scrolled, "line-05") || strings.Contains(scrolled, "line-59") {
		t.Errorf("scrolled view wrong:\n%s", scrolled)
	}
	if !strings.Contains(scrolled, "lines below") {
		t.Errorf("scrolled view missing the follow indicator:\n%s", scrolled)
	}

	// Excess PgUp clamps at the top instead of showing nothing.
	for i := 0; i < 30; i++ {
		m = apply(t, m, tea.KeyMsg{Type: tea.KeyPgUp})
	}
	top := m.viewChatColumn(80, 18)
	if !strings.Contains(top, "line-00") {
		t.Errorf("top view wrong:\n%s", top)
	}

	// Sending a message snaps back to the live tail.
	m.input.SetValue("hello")
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.chatScroll != 0 {
		t.Errorf("chatScroll = %d after send", m.chatScroll)
	}
}

// TestBarCellsNeverPanics: strings.Repeat panics on a negative count, and
// the token bar renders outside any recover — so a bad ratio would take
// the whole TUI down rather than drawing a wrong bar.
func TestBarCellsNeverPanics(t *testing.T) {
	tests := []struct {
		name                     string
		barW, supEquiv, wrkFresh int
		want                     int
	}{
		{"even split", 40, 100, 100, 20},
		{"all supervisor", 40, 100, 0, 40},
		{"all workers", 40, 0, 100, 0},
		{"tiny supervisor share still shows", 40, 1, 100000, 1},
		{"negative worker count", 40, 100, -50, 40},
		{"negative supervisor count", 40, -100, 500, 0},
		{"both negative", 40, -100, -50, 0},
		{"negative width", -5, 100, 100, 0},
		{"zero total", 40, 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := barCells(tt.barW, tt.supEquiv, tt.wrkFresh)
			if got != tt.want {
				t.Errorf("barCells(%d,%d,%d) = %d, want %d",
					tt.barW, tt.supEquiv, tt.wrkFresh, got, tt.want)
			}
			if got < 0 || got > max(tt.barW, 0) {
				t.Fatalf("%d is outside [0,%d] — strings.Repeat would panic", got, tt.barW)
			}
		})
	}
}

// TestCtxGaugeUsesModelWindow: the gauge went red at 115k on a
// 1M-context model, because the threshold was a fixed 100k written when
// 200k was the only window there was.
func TestCtxGaugeUsesModelWindow(t *testing.T) {
	tests := []struct {
		name     string
		window   int // 0 = never reported
		ctx      int
		wantWin  int
		wantWarn bool
	}{
		{"unknown window falls back to 200k", 0, 50_000, 200_000, false},
		{"unknown window warns at half", 0, 120_000, 200_000, true},
		{"1M model at 115k is fine", 1_000_000, 115_000, 1_000_000, false},
		{"1M model at 400k is fine", 1_000_000, 400_000, 1_000_000, false},
		{"1M model warns past half", 1_000_000, 600_000, 1_000_000, true},
		{"200k model warns past half", 200_000, 115_000, 200_000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{supCtxWindow: tt.window}
			if got := m.ctxWindow(); got != tt.wantWin {
				t.Errorf("ctxWindow() = %d, want %d", got, tt.wantWin)
			}
			if got := tt.ctx >= m.ctxWarnAt(); got != tt.wantWarn {
				t.Errorf("ctx %d warn = %v, want %v (threshold %d)",
					tt.ctx, got, tt.wantWarn, m.ctxWarnAt())
			}
		})
	}
}

// TestSupUsageLearnsWindow: the window arrives with the turn's usage and
// must be applied before the same message's context is judged, or the
// first turn on a 1M model still warns.
func TestSupUsageLearnsWindow(t *testing.T) {
	m := New(make(chan tea.Msg))
	next, _ := m.Update(SupUsageMsg{Turns: 1, Ctx: 300_000, CtxWindow: 1_000_000})
	got := next.(Model)
	if got.ctxWindow() != 1_000_000 {
		t.Errorf("window = %d, want 1000000", got.ctxWindow())
	}
	if got.ctxWarned {
		t.Error("warned at 300k of a 1M window")
	}
	// A later 0 must not erase what was learned.
	next2, _ := got.Update(SupUsageMsg{Turns: 1, Ctx: 310_000})
	if got := next2.(Model).ctxWindow(); got != 1_000_000 {
		t.Errorf("window after unreported update = %d, want 1000000", got)
	}
}

// TestReplayedFailureStaysSilent: restarting after a crash reads back
// every worker the crash killed. Announcing those claims they are failing
// now — the bug was a startup toast reading "w192 failed — aborted:
// terminated signal received" for a worker that died with the previous
// process.
func TestReplayedFailureStaysSilent(t *testing.T) {
	tests := []struct {
		name      string
		replay    bool
		wantToast bool
	}{
		{"live failure rings", false, true},
		{"replayed failure is silent", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New(make(chan tea.Msg))
			next, _ := m.Update(WorkerUpsertMsg{ID: "w192", Model: "qwen-dsh", Status: "running"})
			next, _ = next.(Model).Update(WorkerUpsertMsg{
				ID: "w192", Status: "failed",
				Summary: "aborted: terminated signal received",
				Replay:  tt.replay,
			})
			got := next.(Model)
			if (got.toast != "") != tt.wantToast {
				t.Errorf("toast = %q, want any=%v", got.toast, tt.wantToast)
			}
			// Either way the row records the failure and the logs tab keeps it.
			if w := got.worker("w192"); w == nil || w.Status != "failed" {
				t.Error("failure not recorded on the worker row")
			}
			if len(got.logs) == 0 {
				t.Error("failure missing from the logs tab")
			}
		})
	}
}
