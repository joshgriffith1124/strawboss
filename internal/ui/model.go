package ui

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

const (
	tabChat = iota
	tabDashboard
	tabLogs
)

// chatItem is one block in the chat log.
type chatItem struct {
	kind string // "user", "sup", "tool-out", "tool-in", "note"
	when time.Time
	text string
	// tool fields
	toolID  string
	isError bool
}

// workerRow is the UI's view of one worker.
type workerRow struct {
	ID      string
	Model   string
	Task    string
	Status  string
	Summary string
	LogPath string
	Started time.Time
	Ended   time.Time
	In, Out int
}

type modelStat struct {
	Name   string
	TokSec float64
	Active int
	Queue  int
	Note   string
}

// workerEvent is one transcript line in a worker's detail pane. Streamed
// text/reasoning accumulates in a not-done tail entry; completed lines
// (newline-terminated or force-wrapped) are flushed as done entries so the
// transcript scrolls instead of rewriting one line forever.
type workerEvent struct {
	kind string // "text", "reasoning", "tool", "error"
	text string
	done bool
}

// flushWidth force-breaks unbroken streamed text so long paragraphs still
// scroll line by line.
const flushWidth = 160

// appendStream grows the tail entry with a delta and flushes any completed
// lines out of it.
func appendStream(evs []workerEvent, kind, delta string) []workerEvent {
	n := len(evs)
	if n > 0 && !evs[n-1].done && evs[n-1].kind == kind {
		evs[n-1].text += delta
	} else {
		if n > 0 && !evs[n-1].done {
			evs[n-1].done = true // kind changed mid-stream
		}
		evs = append(evs, workerEvent{kind: kind, text: delta})
	}

	// Flush completed lines from the growing tail.
	tail := &evs[len(evs)-1]
	for {
		text := tail.text
		nl := strings.IndexByte(text, '\n')
		var line, rest string
		switch {
		case nl >= 0 && nl <= flushWidth:
			line, rest = text[:nl], text[nl+1:]
		case len(text) > flushWidth:
			cut := flushWidth
			if sp := strings.LastIndexByte(text[:flushWidth], ' '); sp > flushWidth/2 {
				cut = sp
			}
			line, rest = text[:cut], strings.TrimLeft(text[cut:], " ")
		default:
			return evs
		}
		if strings.TrimSpace(line) == "" {
			tail.text = rest
			continue
		}
		kind := tail.kind
		*tail = workerEvent{kind: kind, text: line, done: true}
		evs = append(evs, workerEvent{kind: kind, text: rest})
		tail = &evs[len(evs)-1]
	}
}

// Model is the root Bubble Tea model.
type Model struct {
	feed <-chan tea.Msg

	// OnPrompt/OnInterrupt connect user input to a live supervisor (M5).
	// When nil, the UI notes that no supervisor is attached (demo mode).
	OnPrompt    func(text string)
	OnInterrupt func()
	// OnKillWorker/OnRetryWorker connect the dashboard's worker actions to
	// the live orchestrator; nil in demo mode.
	OnKillWorker  func(id string)
	OnRetryWorker func(id string)

	width, height int
	tab           int
	pulse         bool
	started       time.Time
	now           time.Time

	// supervisor
	sessionID string
	supModel  string
	auth      string
	pid       int
	supStatus string // "✻ …" line
	streaming strings.Builder
	chat      []chatItem
	input     textinput.Model

	// token economy
	supIn, supOut, supCacheRead, supCacheWrite int
	supCost                                    float64
	supTurns                                   int
	fiveHour, sevenDay                         float64
	delegationResultTokens                     []int // per-result estimate, for the avg

	// workers
	workers      []workerRow
	workerEvents map[string][]workerEvent
	selected     int
	follow       bool

	// models
	models []modelStat

	// logs + toast
	logs       []string
	toast      string
	toastUntil time.Time

	quitting bool
}

// New builds the UI over a feed of typed tea.Msgs.
func New(feed <-chan tea.Msg) Model {
	in := textinput.New()
	in.Prompt = sTealB.Render("› ")
	in.Placeholder = ""
	in.Focus()
	return Model{
		feed:         feed,
		started:      time.Now(),
		now:          time.Now(),
		workerEvents: map[string][]workerEvent{},
		follow:       true,
		input:        in,
		auth:         "starting…",
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(Listen(m.feed), tick())
}

func (m Model) worker(id string) *workerRow {
	for i := range m.workers {
		if m.workers[i].ID == id {
			return &m.workers[i]
		}
	}
	return nil
}

func (m *Model) log(source, line string) {
	m.logs = append(m.logs, fmt.Sprintf("%s %-3s %s", time.Now().Format("15:04:05"), source, line))
	if len(m.logs) > 2000 {
		m.logs = m.logs[len(m.logs)-2000:]
	}
}

func (m *Model) flushStreaming() {
	if m.streaming.Len() > 0 {
		m.chat = append(m.chat, chatItem{kind: "sup", when: time.Now(), text: m.streaming.String()})
		m.streaming.Reset()
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.updateKeys(msg)

	case tickMsg:
		m.now = time.Time(msg)
		m.pulse = !m.pulse
		if m.toast != "" && m.now.After(m.toastUntil) {
			m.toast = ""
		}
		return m, tick()

	// ── feed messages ─────────────────────────────────────────────
	case SupInitMsg:
		m.sessionID, m.supModel, m.auth, m.pid = msg.SessionID, msg.Model, msg.Auth, msg.PID
		m.log("sup", "session "+msg.SessionID+" · "+msg.Model+" · "+msg.Auth)
		return m, Listen(m.feed)
	case SupUserMsg:
		m.chat = append(m.chat, chatItem{kind: "user", when: msg.Time, text: msg.Text})
		m.log("sup", "you: "+truncPlain(msg.Text, 120))
		return m, Listen(m.feed)
	case SupTextDeltaMsg:
		m.streaming.WriteString(msg.Text)
		return m, Listen(m.feed)
	case SupTextDoneMsg:
		m.streaming.Reset()
		if strings.TrimSpace(msg.Text) != "" {
			m.chat = append(m.chat, chatItem{kind: "sup", when: msg.Time, text: msg.Text})
			m.log("sup", "supervisor: "+truncPlain(msg.Text, 120))
		}
		return m, Listen(m.feed)
	case SupToolMsg:
		m.flushStreaming()
		text := msg.Command
		if msg.Delegate != nil {
			text = msg.Delegate.Model + " " + fmt.Sprintf("%q", truncPlain(msg.Delegate.Task, 220))
		}
		m.chat = append(m.chat, chatItem{kind: "tool-out", when: time.Now(), toolID: msg.ToolID,
			text: msg.Name + " " + text})
		m.log("sup", "→ "+msg.Name+" "+truncPlain(msg.Command, 120))
		return m, Listen(m.feed)
	case SupToolResultMsg:
		// Store generously; the renderer wraps and caps for display.
		m.chat = append(m.chat, chatItem{kind: "tool-in", when: time.Now(), toolID: msg.ToolID,
			text: truncPlain(msg.Content, 600), isError: msg.IsError})
		m.delegationResultTokens = append(m.delegationResultTokens, len(msg.Content)/4)
		m.log("sup", "← "+truncPlain(msg.Content, 120))
		return m, Listen(m.feed)
	case SupStatusMsg:
		m.supStatus = msg.Status
		return m, Listen(m.feed)
	case SupUsageMsg:
		m.supIn += msg.Input
		m.supOut += msg.Output
		m.supCacheRead += msg.CacheRead
		m.supCacheWrite += msg.CacheWrite
		m.supCost += msg.CostUSD
		m.supTurns += msg.Turns
		return m, Listen(m.feed)
	case SupRateLimitMsg:
		m.fiveHour, m.sevenDay = msg.FiveHour, msg.SevenDay
		return m, Listen(m.feed)
	case SupTurnDoneMsg:
		m.flushStreaming()
		m.supStatus = ""
		if msg.Err != "" {
			m.chat = append(m.chat, chatItem{kind: "note", when: time.Now(), text: "supervisor error: " + msg.Err, isError: true})
			m.ringBell("supervisor error")
		} else if msg.Interrupted {
			m.chat = append(m.chat, chatItem{kind: "note", when: time.Now(), text: "turn interrupted"})
		}
		return m, Listen(m.feed)

	case WorkerUpsertMsg:
		w := m.worker(msg.ID)
		if w == nil {
			m.workers = append(m.workers, workerRow{ID: msg.ID, Started: msg.Started})
			w = &m.workers[len(m.workers)-1]
			if w.Started.IsZero() {
				w.Started = time.Now()
			}
		}
		if msg.Model != "" {
			w.Model = msg.Model
		}
		if msg.Task != "" {
			w.Task = msg.Task
		}
		if msg.Summary != "" {
			w.Summary = msg.Summary
		}
		if msg.LogPath != "" {
			w.LogPath = msg.LogPath
		}
		if msg.Status != "" && msg.Status != w.Status {
			w.Status = msg.Status
			if msg.Status == "done" || msg.Status == "failed" {
				w.Ended = msg.Ended
				if w.Ended.IsZero() {
					w.Ended = time.Now()
				}
			}
			if msg.Status == "failed" {
				m.ringBell(msg.ID + " failed — " + truncPlain(firstLine(msg.Summary), 40))
			}
			m.log("wrk", msg.ID+" "+msg.Status)
		}
		return m, Listen(m.feed)
	case WorkerUsageMsg:
		if w := m.worker(msg.ID); w != nil {
			w.In, w.Out = msg.Input, msg.Output
		}
		return m, Listen(m.feed)
	case WorkerEventMsg:
		evs := m.workerEvents[msg.ID]
		if msg.Kind == "text" || msg.Kind == "reasoning" {
			// Stream deltas accumulate and flush as scrolling lines; they
			// never hit the logs tab (fragment spew).
			evs = appendStream(evs, msg.Kind, msg.Text)
		} else {
			if n := len(evs); n > 0 && !evs[n-1].done {
				evs[n-1].done = true // a discrete event ends any growing line
			}
			evs = append(evs, workerEvent{kind: msg.Kind, text: msg.Text, done: true})
			m.log("wrk", msg.ID+" "+msg.Kind+": "+truncPlain(msg.Text, 110))
		}
		if len(evs) > 200 {
			evs = evs[len(evs)-200:]
		}
		m.workerEvents[msg.ID] = evs
		return m, Listen(m.feed)

	case ModelStatMsg:
		found := false
		for i := range m.models {
			if m.models[i].Name == msg.Name {
				m.models[i] = modelStat(msg)
				found = true
			}
		}
		if !found {
			m.models = append(m.models, modelStat(msg))
			sort.Slice(m.models, func(i, j int) bool { return m.models[i].Name < m.models[j].Name })
		}
		return m, Listen(m.feed)

	case RawLogMsg:
		m.log(msg.Source, msg.Line)
		return m, Listen(m.feed)
	case ToastMsg:
		m.showToast(msg.Text)
		m.log("app", msg.Text)
		return m, Listen(m.feed)

	case SendPromptMsg:
		if m.OnPrompt != nil {
			m.OnPrompt(msg.Text)
		} else {
			m.chat = append(m.chat, chatItem{kind: "note", when: time.Now(),
				text: "demo replay — input isn't wired to a live supervisor yet (M5)"})
		}
		return m, nil
	case InterruptMsg:
		if m.OnInterrupt != nil {
			m.OnInterrupt()
		} else {
			m.chat = append(m.chat, chatItem{kind: "note", when: time.Now(), text: "esc-to-interrupt lands in M5"})
		}
		return m, nil
	case KillWorkerMsg:
		if m.OnKillWorker != nil {
			m.OnKillWorker(msg.ID)
		} else {
			m.showToast("demo replay — worker kill isn't wired to a live harness")
		}
		return m, nil
	case RetryWorkerMsg:
		if m.OnRetryWorker != nil {
			m.OnRetryWorker(msg.ID)
		} else {
			m.showToast("demo replay — worker retry isn't wired to a live harness")
		}
		return m, nil

	case nil:
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// ringBell shows the failure toast and rings the terminal bell — the
// point is NOT watching the dashboard.
func (m *Model) ringBell(text string) {
	m.toast = "⚠ " + text
	m.toastUntil = time.Now().Add(8 * time.Second)
	os.Stdout.WriteString("\a")
}

// showToast shows a transient status line without the bell.
func (m *Model) showToast(text string) {
	m.toast = text
	m.toastUntil = time.Now().Add(5 * time.Second)
}

// selectedWorker resolves the dashboard selection against display order.
func (m Model) selectedWorker() *workerRow {
	rows := m.sortedWorkers()
	if m.selected >= 0 && m.selected < len(rows) {
		return &rows[m.selected]
	}
	return nil
}

func (m Model) updateKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	typing := m.tab == tabChat

	switch msg.String() {
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "tab":
		m.tab = (m.tab + 1) % 3
		return m, nil
	case "shift+tab":
		m.tab = (m.tab + 2) % 3
		return m, nil
	case "esc":
		if m.tab == tabChat {
			return m, func() tea.Msg { return InterruptMsg{} }
		}
		m.tab = tabChat
		return m, nil
	}

	if typing {
		switch msg.String() {
		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			m.input.SetValue("")
			m.chat = append(m.chat, chatItem{kind: "user", when: time.Now(), text: text})
			return m, func() tea.Msg { return SendPromptMsg{Text: text} }
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	// non-typing tabs
	switch msg.String() {
	case "q":
		m.quitting = true
		return m, tea.Quit
	case "1":
		m.tab = tabChat
	case "2":
		m.tab = tabDashboard
	case "3":
		m.tab = tabLogs
	case "up", "k":
		if m.tab == tabDashboard && m.selected > 0 {
			m.selected--
		}
	case "down", "j":
		if m.tab == tabDashboard && m.selected < len(m.workers)-1 {
			m.selected++
		}
	case "f":
		m.follow = !m.follow
	case "x":
		if m.tab != tabDashboard {
			break
		}
		wk := m.selectedWorker()
		if wk == nil {
			break
		}
		if wk.Status != "running" && wk.Status != "queued" {
			m.showToast(wk.ID + " isn't running — nothing to kill")
			break
		}
		id := wk.ID
		return m, func() tea.Msg { return KillWorkerMsg{ID: id} }
	case "r":
		if m.tab != tabDashboard {
			break
		}
		wk := m.selectedWorker()
		if wk == nil {
			break
		}
		if wk.Status != "done" && wk.Status != "failed" {
			m.showToast(wk.ID + " is still running — kill it first (x)")
			break
		}
		id := wk.ID
		return m, func() tea.Msg { return RetryWorkerMsg{ID: id} }
	}
	return m, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// sortedWorkers returns the display order: queued+running first (newest
// spawn last, like the mock's live rows), then finished by end time desc.
func (m Model) sortedWorkers() []workerRow {
	out := make([]workerRow, len(m.workers))
	copy(out, m.workers)
	rank := func(w workerRow) int {
		switch w.Status {
		case "queued":
			return 0
		case "running":
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}
