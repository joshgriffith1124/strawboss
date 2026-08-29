package ui

import (
	"fmt"
	"os"
	"regexp"
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
	Dir     string
	Started time.Time
	Ended   time.Time
	In, Out int
	Ctx     int // current context footprint (0 = unknown)
	Steps   int // completed model steps (dsh; 0 = unknown)
}

// workerRate is per-worker throughput derived from usage deltas.
type workerRate struct {
	out  int
	at   time.Time
	rate float64   // smoothed output tok/s
	hist []float64 // rate samples for the sparkline (capped)
}

// resultLine is one recent delegation result for the supervisor pane.
type resultLine struct {
	text    string
	isError bool
}

// modelStat mirrors ModelStatMsg field-for-field (conversion below).
type modelStat struct {
	Name          string
	TokSec        float64
	Active        int
	Queue         int
	Note          string
	ContextWindow int
}

// supTokens is the display view of supervisor usage: committed turn
// totals plus the running turn's live estimate.
func (m Model) supTokens() (in, cacheRead, cacheWrite, out int) {
	return m.supIn + m.turnIn, m.supCacheRead + m.turnCacheRead,
		m.supCacheWrite + m.turnCacheWrite, m.supOut + m.turnOut
}

// contextWindowFor is the model's reported context length, 0 if unknown.
func (m Model) contextWindowFor(name string) int {
	for _, ms := range m.models {
		if ms.Name == name {
			return ms.ContextWindow
		}
	}
	return 0
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
	// OnListSessions/OnSwitchSession drive the session picker.
	OnListSessions  func() []SessionInfo
	OnSwitchSession func(id, run string)

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

	// token economy: committed totals (from turn results) plus a live
	// bucket for the running turn (per-call estimates, replaced by the
	// authoritative result when the turn ends).
	supIn, supOut, supCacheRead, supCacheWrite     int
	turnIn, turnOut, turnCacheRead, turnCacheWrite int
	supCost                                        float64
	supTurns                                       int
	fiveHour, sevenDay                             float64
	delegationResultTokens                         []int        // per-result estimate, for the avg
	recentResults                                  []resultLine // last delegation results (capped)

	// workers
	workers      []workerRow
	workerEvents map[string][]workerEvent
	workerRates  map[string]workerRate
	selected     int
	follow       bool
	filterInput  textinput.Model
	filtering    bool   // filter input focused
	filter       string // applied worker-table filter

	// session picker overlay
	picking  bool
	sessions []SessionInfo
	sessIdx  int

	// models
	models []modelStat

	deniedSeen map[string]bool // allowlist suggestions already shown

	// logs + toast
	logs          []logLine
	logSrc        string // logs-tab source filter: "" all, else sup/wrk/app
	toast         string
	toastUntil    time.Time
	remoteChannel string // armed two-way channel ("" = none)

	quitting bool
}

// New builds the UI over a feed of typed tea.Msgs.
func New(feed <-chan tea.Msg) Model {
	in := textinput.New()
	in.Prompt = sTealB.Render("› ")
	in.Placeholder = ""
	in.Focus()
	filter := textinput.New()
	filter.Prompt = sTealB.Render("/ ")
	return Model{
		feed:         feed,
		started:      time.Now(),
		now:          time.Now(),
		workerEvents: map[string][]workerEvent{},
		workerRates:  map[string]workerRate{},
		follow:       true,
		input:        in,
		filterInput:  filter,
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

// logLine is one logs-tab entry, kept with its source for filtering.
type logLine struct {
	src  string // "sup", "wrk", "app"
	text string // fully formatted line
}

func (m *Model) log(source, line string) {
	m.logs = append(m.logs, logLine{src: source,
		text: fmt.Sprintf("%s %-3s %s", time.Now().Format("15:04:05"), source, line)})
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
	case feedBatch:
		// Fold the whole batch, render once, re-arm once. Every feed
		// case below returns Listen; the intermediate cmds are dropped.
		var model tea.Model = m
		for _, inner := range msg {
			model, _ = model.Update(inner)
		}
		return model, Listen(m.feed)

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
		// Delegation results (terse "wN status …" lines and budget
		// refusals) also feed the supervisor pane's recent list.
		if fl := firstLine(msg.Content); isDelegationResult(fl) {
			m.recentResults = append(m.recentResults, resultLine{text: fl, isError: msg.IsError})
			if len(m.recentResults) > 5 {
				m.recentResults = m.recentResults[len(m.recentResults)-5:]
			}
		}
		// Permission denials are silent auto-denies in dontAsk mode; make
		// them LOUD with a ready-made allowlist fix, once per suggestion.
		if tool := DeniedTool(msg.Content); tool != "" {
			cmd := ""
			for i := len(m.chat) - 1; i >= 0; i-- {
				if m.chat[i].kind == "tool-out" && m.chat[i].toolID == msg.ToolID {
					cmd = m.chat[i].text
					break
				}
			}
			if sug := AllowSuggestion(tool, cmd); sug != "" && !m.deniedSeen[sug] {
				if m.deniedSeen == nil {
					m.deniedSeen = map[string]bool{}
				}
				m.deniedSeen[sug] = true
				m.chat = append(m.chat, chatItem{kind: "note", isError: true, when: time.Now(),
					text: fmt.Sprintf("⛔ supervisor was denied %s — to allow permanently, add %q to supervisor.allowed_tools in ~/.strawboss/config.toml", tool, sug)})
				m.showToast("supervisor denied " + tool + " — see chat for the allowlist fix")
			}
		}
		m.log("sup", "← "+truncPlain(msg.Content, 120))
		return m, Listen(m.feed)
	case SupStatusMsg:
		m.supStatus = msg.Status
		return m, Listen(m.feed)
	case SupTurnUsageMsg:
		m.turnIn += msg.Input
		m.turnOut += msg.Output
		m.turnCacheRead += msg.CacheRead
		m.turnCacheWrite += msg.CacheWrite
		return m, Listen(m.feed)
	case SupUsageMsg:
		// Authoritative turn totals: commit them and drop the live
		// estimate so nothing double-counts.
		m.supIn += msg.Input
		m.supOut += msg.Output
		m.supCacheRead += msg.CacheRead
		m.supCacheWrite += msg.CacheWrite
		m.supCost += msg.CostUSD
		m.supTurns += msg.Turns
		m.turnIn, m.turnOut, m.turnCacheRead, m.turnCacheWrite = 0, 0, 0, 0
		return m, Listen(m.feed)
	case SupRateLimitMsg:
		m.fiveHour, m.sevenDay = msg.FiveHour, msg.SevenDay
		return m, Listen(m.feed)
	case SupTurnDoneMsg:
		m.flushStreaming()
		m.supStatus = ""
		// A turn that died without a result (interrupt, crash) keeps its
		// per-call estimates as the best available record.
		m.supIn += m.turnIn
		m.supOut += m.turnOut
		m.supCacheRead += m.turnCacheRead
		m.supCacheWrite += m.turnCacheWrite
		m.turnIn, m.turnOut, m.turnCacheRead, m.turnCacheWrite = 0, 0, 0, 0
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
		if msg.Dir != "" {
			w.Dir = msg.Dir
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
			// Live tok/s from output deltas, lightly smoothed; the models
			// panel shows instantaneous per-endpoint rate, this one is the
			// worker's own.
			now := time.Now()
			r := m.workerRates[msg.ID]
			if !r.at.IsZero() && msg.Output > r.out && now.After(r.at) {
				inst := float64(msg.Output-r.out) / now.Sub(r.at).Seconds()
				if r.rate > 0 {
					r.rate = 0.6*r.rate + 0.4*inst
				} else {
					r.rate = inst
				}
			}
			if r.rate > 0 {
				r.hist = append(r.hist, r.rate)
				if len(r.hist) > 120 {
					r.hist = r.hist[len(r.hist)-120:]
				}
			}
			r.out, r.at = msg.Output, now
			m.workerRates[msg.ID] = r
			w.In, w.Out = msg.Input, msg.Output
			if msg.Ctx > 0 {
				w.Ctx = msg.Ctx
				// dsh emits exactly one context-bearing usage per model
				// step, so these double as a step counter.
				w.Steps++
			}
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
			if !msg.Replay {
				m.log("wrk", msg.ID+" "+msg.Kind+": "+truncPlain(msg.Text, 110))
			}
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
	case RemoteMsg:
		m.remoteChannel = msg.Channel
		m.log("app", "remote control armed via "+msg.Channel)
		return m, Listen(m.feed)
	case SessionSwitchedMsg:
		// Old session's chat and workers no longer apply; the registry
		// replay repopulates the new run's workers.
		m.chat = nil
		m.streaming.Reset()
		m.workers = nil
		m.workerEvents = map[string][]workerEvent{}
		m.workerRates = map[string]workerRate{}
		m.recentResults = nil
		m.selected = 0
		m.sessionID = msg.ID
		m.chat = append(m.chat, chatItem{kind: "note", when: time.Now(),
			text: "switched to session " + msg.ID + " — the conversation resumes on your next message"})
		m.showToast("switched to session " + truncPlain(msg.ID, 12))
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
	rows := m.visibleWorkers()
	if m.selected >= 0 && m.selected < len(rows) {
		return &rows[m.selected]
	}
	return nil
}

// visibleWorkers is display order with the table filter applied: a
// case-insensitive substring match across id, model, status, task, and
// summary. "!" alone means running, "x" alone means failed.
func (m Model) visibleWorkers() []workerRow {
	rows := m.sortedWorkers()
	f := strings.ToLower(strings.TrimSpace(m.filter))
	if f == "" {
		return rows
	}
	switch f {
	case "!":
		f = "running"
	case "x":
		f = "failed"
	}
	var out []workerRow
	for _, wk := range rows {
		hay := strings.ToLower(wk.ID + " " + wk.Model + " " + wk.Status + " " + wk.Task + " " + wk.Summary)
		if strings.Contains(hay, f) {
			out = append(out, wk)
		}
	}
	return out
}

func (m Model) updateKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	typing := m.tab == tabChat

	// The session picker owns the keyboard while open.
	if m.picking {
		switch msg.String() {
		case "esc", "q", "s":
			m.picking = false
		case "up", "k":
			if m.sessIdx > 0 {
				m.sessIdx--
			}
		case "down", "j":
			if m.sessIdx < len(m.sessions)-1 {
				m.sessIdx++
			}
		case "enter":
			m.picking = false
			if m.sessIdx < len(m.sessions) {
				s := m.sessions[m.sessIdx]
				if s.Current {
					m.showToast("already on that session")
				} else if m.OnSwitchSession != nil {
					m.OnSwitchSession(s.ID, s.Run)
				} else {
					m.showToast("demo replay — session switching isn't wired")
				}
			}
		}
		return m, nil
	}

	// Filter entry owns the keyboard while focused.
	if m.filtering {
		switch msg.String() {
		case "enter":
			m.filter = strings.TrimSpace(m.filterInput.Value())
			m.filtering = false
			m.selected = 0
			return m, nil
		case "esc":
			m.filtering = false
			m.filter = ""
			m.filterInput.SetValue("")
			return m, nil
		}
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		return m, cmd
	}

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
		if m.tab == tabDashboard && m.filter != "" {
			m.filter = ""
			m.filterInput.SetValue("")
			m.selected = 0
			return m, nil
		}
		m.tab = tabChat
		return m, nil
	}

	if typing {
		switch msg.String() {
		case "enter":
			text := normalizeDroppedPaths(strings.TrimSpace(m.input.Value()))
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
		if m.tab == tabDashboard && m.selected < len(m.visibleWorkers())-1 {
			m.selected++
		}
	case "/":
		if m.tab == tabDashboard {
			m.filtering = true
			m.filterInput.SetValue(m.filter)
			m.filterInput.Focus()
		}
	case "s":
		if m.OnListSessions != nil {
			m.sessions = m.OnListSessions()
			m.sessIdx = 0
			m.picking = true
		} else {
			m.showToast("demo replay — no session history")
		}
	case "f":
		if m.tab == tabLogs {
			// Cycle the logs source filter: all → sup → wrk → app.
			order := []string{"", "sup", "wrk", "app"}
			for i, s := range order {
				if m.logSrc == s {
					m.logSrc = order[(i+1)%len(order)]
					break
				}
			}
		} else {
			m.follow = !m.follow
		}
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
	case "R":
		if m.tab != tabDashboard {
			break
		}
		// Recover-all: retry every FAILED worker in the current (possibly
		// filtered) view — narrow with `/` first to scope the sweep.
		var cmds []tea.Cmd
		for _, wk := range m.visibleWorkers() {
			if wk.Status != "failed" {
				continue
			}
			id := wk.ID
			cmds = append(cmds, func() tea.Msg { return RetryWorkerMsg{ID: id} })
		}
		if len(cmds) == 0 {
			m.showToast("no failed workers in view")
			break
		}
		m.showToast(fmt.Sprintf("retrying %d failed worker(s)", len(cmds)))
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// normalizeDroppedPaths makes drag-and-dropped file paths usable by the
// supervisor: terminals paste the path as text, but on WSL a Windows
// Terminal drop arrives Windows-style ("C:\Users\…", possibly quoted) —
// unreadable from the Linux side. Quoted Windows paths become /mnt/<drive>
// form; everything else passes through untouched.
// Quoted paths may contain spaces (how terminals quote dropped paths);
// bare ones end at whitespace so two pasted paths stay separate.
var winPathRe = regexp.MustCompile(`"([A-Za-z]):\\([^"]+)"|([A-Za-z]):\\([^\s"']+)`)

func normalizeDroppedPaths(s string) string {
	return winPathRe.ReplaceAllStringFunc(s, func(match string) string {
		p := winPathRe.FindStringSubmatch(match)
		drive, rest := p[1], p[2]
		if drive == "" {
			drive, rest = p[3], p[4]
		}
		return "/mnt/" + strings.ToLower(drive) + "/" + strings.ReplaceAll(rest, `\`, "/")
	})
}

// DeniedTool extracts the tool name from a permission-denial tool result
// ("" when the result isn't one). Exported: the orchestrator uses the
// same detection for remote denial notices.
var deniedRe = regexp.MustCompile(`^Permission to use (\w+) has been denied`)

func DeniedTool(content string) string {
	if match := deniedRe.FindStringSubmatch(content); match != nil {
		return match[1]
	}
	return ""
}

// AllowSuggestion builds the allowed_tools entry that would have let the
// denied call through: command-prefixed for Bash, the bare tool otherwise.
func AllowSuggestion(tool, cmd string) string {
	if tool != "Bash" {
		return tool
	}
	// The chat's tool line reads "Bash <command…>"; the first command
	// token drives the prefix rule.
	cmd = strings.TrimSpace(strings.TrimPrefix(cmd, "Bash"))
	if fields := strings.Fields(cmd); len(fields) > 0 {
		return "Bash(" + fields[0] + ":*)"
	}
	return "Bash"
}

// isDelegationResult recognizes the terse-result header ("wN status …")
// and budget refusals among tool results — other tool output (Read,
// Glob…) stays out of the recent-results list.
func isDelegationResult(line string) bool {
	if strings.Contains(line, "blocked by the budget guard") {
		return true
	}
	if len(line) < 3 || line[0] != 'w' {
		return false
	}
	rest := line[1:]
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	return i > 0 && i < len(rest) && rest[i] == ' '
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
