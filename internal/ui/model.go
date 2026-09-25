package ui

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// Top-level tabs, in display order (keys 1-6). tabChat renders the Agents
// screen (supervisor chat pane, worker panes, sidebar, bottom panel);
// tabDashboard is the Tasks screen; tabWorktrees is branch review.
const (
	tabChat = iota
	tabDashboard
	tabWorktrees
	tabModels
	tabAnalytics
	tabLogs
	numTabs
)

// tabNames are the top-bar labels, indexed by tab constant.
var tabNames = [numTabs]string{"Agents", "Tasks", "Worktrees", "Models", "Analytics", "Logs"}

// Agents-screen pane focus: the supervisor pane owns typing; the worker
// focus owns ↑↓ selection and the worker action keys (x r R u).
const (
	focusSup = iota
	focusWork
	focusBottom // the bottom Tasks/Logs/Metrics panel
)

// Agents-screen bottom panel tabs.
const (
	bottomTasks = iota
	bottomLogs
	bottomMetrics
	numBottomTabs
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
	CacheRd int // worker-side prefix re-reads, separate from fresh In
	Ctx     int // current context footprint (0 = unknown)
	Steps   int // completed model steps (dsh; 0 = unknown)
}

// Throughput sampling: rates need at least minRateWindow between samples,
// and a rate older than rateStale no longer counts as live.
const (
	minRateWindow = 500 * time.Millisecond
	rateStale     = 8 * time.Second
)

// liveRate is a worker's smoothed output tok/s while its samples are
// fresh, else 0 — a stalled or dead worker must not keep its last rate.
func (m Model) liveRate(id string) float64 {
	r, ok := m.workerRates[id]
	if !ok || m.now.Sub(r.at) >= rateStale {
		return 0
	}
	return r.rate
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

// The context gauge is a FRACTION of the session model's real window, and
// an UNKNOWN window paints nothing red. A fixed 100k line was written
// when 200k was the only window there was; against a 1M-context model it
// painted the gauge red at 11% full. A guessed 200k denominator was no
// better — it accused a resumed 1M session before its first turn could
// report. The window comes from the result event's modelUsage
// (supervisor.ResultEvent.ModelWindows), is remembered per model across
// runs, and is applied at system/init when known.
const (
	// ctxWarnFrac is where "every call re-reads all of this" stops being
	// background cost and starts being worth a fresh session.
	ctxWarnFrac = 0.5
)

// ctxWindow is the session model's context window; 0 means not known.
func (m Model) ctxWindow() int { return m.supCtxWindow }

// ctxBloated is the gauge's red condition: the footprint has crossed
// ctxWarnFrac of a KNOWN window. Unknown never warns — red is an
// accusation, and it needs a denominator.
func (m Model) ctxBloated() bool {
	w := m.ctxWindow()
	return w > 0 && m.supCtx >= int(float64(w)*ctxWarnFrac)
}

// noteSupCtx records the supervisor's context footprint (0 = unknown,
// keeps existing) and advises a fresh session ONCE when it crosses the
// warning line — covers both a bloated resume and organic growth.
func (m *Model) noteSupCtx(ctx int) {
	if ctx <= 0 {
		return
	}
	m.supCtx = ctx
	if m.ctxBloated() && !m.ctxWarned {
		m.ctxWarned = true
		m.chat = append(m.chat, chatItem{kind: "note", when: time.Now(),
			text: fmt.Sprintf("supervisor context is ~%s tokens — every call re-reads it all; type /new to start a fresh session (s lists past ones)", formatTokens(ctx))})
		m.showToast(fmt.Sprintf("supervisor context ~%s — /new starts fresh", formatTokens(ctx)))
	}
}

// chatPageStep is one PgUp/PgDn stride: most of a screen of chat, with
// overlap for continuity.
func (m Model) chatPageStep() int {
	step := m.height - 8
	if step < 5 {
		step = 5
	}
	return step
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

	// OnPrompt/OnInterrupt connect user input to a live supervisor.
	// When nil, the UI notes that no supervisor is attached (demo mode).
	OnPrompt    func(text string, images []Image)
	OnInterrupt func()
	// OnPasteImage grabs the next clipboard image (ctrl+v / alt+v in
	// chat). It runs OUTSIDE the Update loop (returned as a tea.Cmd) and
	// reports back via ImagePastedMsg; nil in demo mode.
	OnPasteImage func() (Image, error)
	// OnKillWorker/OnRetryWorker connect the dashboard's worker actions to
	// the live orchestrator; nil in demo mode.
	OnKillWorker  func(id string)
	OnRetryWorker func(id string)
	// OnListSessions/OnSwitchSession drive the session picker;
	// OnNewSession starts a fresh session (`/new`, `n`).
	OnListSessions  func() []SessionInfo
	OnSwitchSession func(id, run string)
	OnNewSession    func()
	// OnListBranches/OnBranchDiff/OnMergeBranch/OnDiscardBranch drive the
	// branch review overlay (`b`); nil in demo mode.
	OnListBranches  func() []BranchInfo
	OnBranchDiff    func(name string) (string, error)
	OnMergeBranch   func(name string) error
	OnDiscardBranch func(name string) error
	// OnUndoWorker reverts a finished worker's edits (`u`); nil in demo mode.
	OnUndoWorker func(id, dir string) (files []string, err error)
	// OnScorecard loads the historical per-model scorecard (the `strawboss
	// stats` table) for the Analytics tab. Called on entering the tab, never
	// per frame; nil in demo mode.
	OnScorecard func() []ScoreRow

	width, height int
	tab           int
	focus         int  // Agents screen: focusSup, focusWork or focusBottom
	zoomed        bool // Agents screen: focused pane fills the main area
	hideTiles     bool // Agents screen: worker tiles hidden (ctrl+t); supervisor gets the width
	bottomTab     int  // Agents screen: bottomTasks/bottomLogs/bottomMetrics
	pulse         bool
	started       time.Time
	now           time.Time

	// supervisor
	sessionID string
	supModel  string
	auth      string
	pid       int
	supStatus string // "✻ …" line
	// slashCmds is the supervisor CLI's command list (from system/init):
	// "/name …" typed in chat goes to the CLI verbatim, which expands it
	// like the interactive terminal would. Only /new is handled locally.
	slashCmds []string
	// streaming is the in-progress supervisor reply. A plain string on
	// purpose: Model is passed by value and boxed into tea.Model between
	// updates, and a strings.Builder here panics on the next write after
	// a copy — and leaves a dangling self-pointer in the heap copy that
	// the GC reports as a bad pointer. Both were live crashes (NOTES).
	streaming string
	chat      []chatItem
	// chatScroll is how many lines above the bottom the chat view sits
	// (0 = following the newest content). PgUp/PgDn drive it; sending a
	// message snaps back to the bottom.
	chatScroll int
	input      textinput.Model
	// pending holds images pasted into the chat input; the next prompt
	// sends them, and backspace on an empty input line drops the last.
	pending []Image

	// token economy: committed totals (from turn results) plus a live
	// bucket for the running turn (per-call estimates, replaced by the
	// authoritative result when the turn ends).
	supIn, supOut, supCacheRead, supCacheWrite     int
	turnIn, turnOut, turnCacheRead, turnCacheWrite int
	supCost                                        float64
	supTurns                                       int
	// supCtx is the supervisor's context footprint: the last API call's
	// full prompt size (fresh + cache reads + writes). Every call re-reads
	// the whole prefix, so this is what each future call will cost again.
	supCtx                 int
	supCtxWindow           int  // session model's real window (0 = not yet reported)
	ctxWarned              bool // big-context advice shown once per session
	fiveHour, sevenDay     float64
	delegationResultTokens []int        // per-result estimate, for the avg
	recentResults          []resultLine // last delegation results (capped)

	// workers
	workers      []workerRow
	workerEvents map[string][]workerEvent
	workerRates  map[string]workerRate
	selected     int
	selID        string // pinned selected worker id ("" = use selected as-is)
	follow       bool
	filterInput  textinput.Model
	filtering    bool   // filter input focused
	filter       string // applied worker-table filter

	// session picker overlay
	picking  bool
	sessions []SessionInfo
	sessIdx  int

	// branch review overlay
	reviewing  bool
	branches   []BranchInfo
	brIdx      int
	diffText   string // non-empty while the diff pane is open
	diffScroll int
	brConfirm  string // "m:<name>" or "x:<name>" after a first press; a second press confirms

	// undoConfirm holds the worker id awaiting a second "u" press; any
	// other key press disarms it.
	undoConfirm string

	// scores is the Analytics tab's scorecard, reloaded via OnScorecard
	// each time the tab is entered.
	scores []ScoreRow

	// models
	models []modelStat
	// modelSet is the model names of the latest reload (nil = none yet:
	// accept every name). Replaced whole, never mutated — Model is copied
	// by value, so a shared map must stay read-only.
	modelSet map[string]bool

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
	in.Prompt = sRunB.Render("› ")
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
	if m.streaming != "" {
		m.chat = append(m.chat, chatItem{kind: "sup", when: time.Now(), text: m.streaming})
		m.streaming = ""
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
		if len(msg.SlashCommands) > 0 {
			m.slashCmds = msg.SlashCommands
		}
		m.log("sup", "session "+msg.SessionID+" · "+msg.Model+" · "+msg.Auth)
		return m, Listen(m.feed)
	case SupNoteMsg:
		m.chat = append(m.chat, chatItem{kind: "note", when: time.Now(), text: msg.Text, isError: msg.IsError})
		m.log("sup", msg.Text)
		return m, Listen(m.feed)
	case SupUserMsg:
		m.chat = append(m.chat, chatItem{kind: "user", when: msg.Time, text: msg.Text})
		m.log("sup", "you: "+truncPlain(msg.Text, 120))
		return m, Listen(m.feed)
	case SupTextDeltaMsg:
		m.streaming += msg.Text
		return m, Listen(m.feed)
	case SupTextDoneMsg:
		m.streaming = ""
		if strings.TrimSpace(msg.Text) != "" {
			m.chat = append(m.chat, chatItem{kind: "sup", when: msg.Time, text: msg.Text})
			m.log("sup", "supervisor: "+truncPlain(msg.Text, 120))
		}
		return m, Listen(m.feed)
	case SupToolMsg:
		m.flushStreaming()
		text := msg.Command
		if msg.Delegate != nil {
			switch {
			case msg.Delegate.Task != "":
				text = msg.Delegate.Model + " " + fmt.Sprintf("%q", truncPlain(msg.Delegate.Task, 220))
			case msg.Delegate.TaskFile != "":
				text = msg.Delegate.Model + " · task file " + truncPlain(msg.Delegate.TaskFile, 220)
			default:
				text = msg.Delegate.Model
			}
		}
		m.chat = append(m.chat, chatItem{kind: "tool-out", when: time.Now(), toolID: msg.ToolID,
			text: msg.Name + " " + text})
		m.log("sup", "→ "+msg.Name+" "+truncPlain(msg.Command, 120))
		return m, Listen(m.feed)
	case SupToolResultMsg:
		// Store generously; the renderer wraps and caps for display. The
		// result is shown on one line, so whitespace runs (cat -n line
		// numbers, YAML indentation, newlines) squeeze to single spaces —
		// otherwise a Read of an indented file renders as a few tokens
		// adrift in a sea of blanks.
		m.chat = append(m.chat, chatItem{kind: "tool-in", when: time.Now(), toolID: msg.ToolID,
			text: truncPlain(squeezeSpace(msg.Content, 4000), 600), isError: msg.IsError})
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
		m.noteSupCtx(msg.Input + msg.CacheRead + msg.CacheWrite)
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
		if msg.CtxWindow > 0 {
			m.supCtxWindow = msg.CtxWindow // learned before the gauge is judged
		}
		m.noteSupCtx(msg.Ctx)
		return m, Listen(m.feed)
	case SupCtxWindowMsg:
		if msg.Window > 0 {
			m.supCtxWindow = msg.Window
		}
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
			// Replayed failures stay silent. Restarting after a crash
			// reads back every worker the crash killed, and announcing
			// them claims they are failing now — the same rule the
			// remote push already follows.
			if msg.Status == "failed" && !msg.Replay {
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
			// Samples closer than minRateWindow (a startup replay or a
			// batched burst) would divide real tokens by ~0s: keep the
			// older anchor so the next sample spans a real interval.
			if !r.at.IsZero() && now.Sub(r.at) < minRateWindow {
				w.In, w.Out = msg.Input, msg.Output
				w.CacheRd = msg.CacheRead
				if msg.Ctx > 0 {
					w.Ctx = msg.Ctx
					w.Steps++
				}
				return m, Listen(m.feed)
			}
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
			w.CacheRd = msg.CacheRead
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
		if m.modelSet != nil && !m.modelSet[msg.Name] {
			// A poll pass that began before a reload reporting a model the
			// reload removed: re-adding it would leave a ghost row.
			return m, Listen(m.feed)
		}
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

	case ModelSetMsg:
		keep := make(map[string]bool, len(msg.Names))
		for _, n := range msg.Names {
			keep[n] = true
		}
		kept := m.models[:0:0] // fresh backing array: m is copied by value
		for _, ms := range m.models {
			if keep[ms.Name] {
				kept = append(kept, ms)
			}
		}
		m.models = kept
		m.modelSet = keep
		return m, Listen(m.feed)

	case RawLogMsg:
		m.log(msg.Source, msg.Line)
		return m, Listen(m.feed)
	case ToastMsg:
		m.showToast(msg.Text) // showToast logs it
		return m, Listen(m.feed)
	case RemoteMsg:
		m.remoteChannel = msg.Channel
		m.log("app", "remote control armed via "+msg.Channel)
		return m, Listen(m.feed)
	case SessionSwitchedMsg:
		// Old session's chat, workers, and supervisor totals no longer
		// apply; the registry replay and the run's persisted supervisor
		// ledger repopulate the new run's numbers.
		m.supIn, m.supOut, m.supCacheRead, m.supCacheWrite = 0, 0, 0, 0
		m.turnIn, m.turnOut, m.turnCacheRead, m.turnCacheWrite = 0, 0, 0, 0
		m.supCost, m.supTurns = 0, 0
		m.supCtx, m.ctxWarned = 0, false
		m.chat = nil
		m.streaming = ""
		m.workers = nil
		m.workerEvents = map[string][]workerEvent{}
		m.workerRates = map[string]workerRate{}
		m.recentResults = nil
		m.selected, m.selID = 0, ""
		m.sessionID = msg.ID
		if msg.ID == "" {
			m.chat = append(m.chat, chatItem{kind: "note", when: time.Now(),
				text: "fresh session — your next message starts a new conversation with an empty context"})
			m.showToast("fresh session")
		} else {
			m.chat = append(m.chat, chatItem{kind: "note", when: time.Now(),
				text: "switched to session " + msg.ID + " — the conversation resumes on your next message"})
			m.showToast("switched to session " + truncPlain(msg.ID, 12))
		}
		return m, Listen(m.feed)

	case SendPromptMsg:
		if m.OnPrompt != nil {
			m.OnPrompt(msg.Text, msg.Images)
		} else {
			m.chat = append(m.chat, chatItem{kind: "note", when: time.Now(),
				text: "demo replay — input isn't wired to a live supervisor"})
		}
		return m, nil
	case ImagePastedMsg:
		if errors.Is(msg.Err, ErrNoImage) && msg.Key == "ctrl+v" {
			// Terminals that pass ctrl+v through (rather than pasting
			// themselves) would otherwise lose plain text paste.
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
			return m, cmd
		}
		if msg.Err != nil {
			m.showToast("paste: " + msg.Err.Error())
			return m, nil
		}
		m.pending = append(m.pending, msg.Image)
		m.showToast(fmt.Sprintf("image attached · %d×%d · %d KB — backspace on an empty line removes it",
			msg.Image.Width, msg.Image.Height, len(msg.Image.Data)/1024))
		return m, nil
	case NewSessionMsg:
		if m.OnNewSession != nil {
			m.OnNewSession()
		} else {
			m.showToast("demo replay — fresh sessions aren't wired to a live supervisor")
		}
		return m, nil
	case InterruptMsg:
		if m.OnInterrupt != nil {
			m.OnInterrupt()
		} else {
			m.chat = append(m.chat, chatItem{kind: "note", when: time.Now(), text: "demo replay — interrupt isn't wired to a live supervisor"})
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

// slashMatches lists the commands the chat input could complete to: the
// local /new plus the supervisor's list, prefix-matched against the input
// while it is still a bare "/word" (no arguments yet). Empty otherwise.
func (m Model) slashMatches() []string {
	v := m.input.Value()
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \t") {
		return nil
	}
	var out []string
	for _, c := range append([]string{"new"}, m.slashCmds...) {
		if strings.HasPrefix("/"+c, v) {
			out = append(out, "/"+c)
		}
	}
	return out
}

// completeSlash fills the input with the first matching command and a
// trailing space, ready for arguments. Reports false when there was
// nothing to complete so Tab keeps its tab-switching job.
func (m Model) completeSlash() (bool, Model) {
	matches := m.slashMatches()
	if len(matches) == 0 {
		return false, m
	}
	m.input.SetValue(matches[0] + " ")
	m.input.CursorEnd()
	return true, m
}

// ringBell shows the failure toast and rings the terminal bell — the
// point is NOT watching the dashboard.
func (m *Model) ringBell(text string) {
	m.toast = "⚠ " + text
	m.toastUntil = time.Now().Add(8 * time.Second)
	os.Stdout.WriteString("\a")
}

// showToast shows a transient status line without the bell. Every toast is
// also written to the logs tab: a message that shows for five seconds and
// then vanishes is unreportable otherwise — the same reason crashes go to
// a file rather than a screen the TUI is about to restore.
func (m *Model) showToast(text string) {
	m.toast = text
	m.toastUntil = time.Now().Add(5 * time.Second)
	m.log("app", text)
}

// selectedWorker resolves the dashboard selection against display order.
func (m Model) selectedWorker() *workerRow {
	rows := m.visibleWorkers()
	if i := m.selIndex(rows); i >= 0 && i < len(rows) {
		return &rows[i]
	}
	return nil
}

// selIndex is the selected worker's row in rows (the visible list). The
// selection is anchored to a worker id: the display order shifts as
// workers start and finish, and a bare index would drift onto another
// worker. Falls back to the index while no id is pinned or the pinned
// worker is filtered out.
func (m Model) selIndex(rows []workerRow) int {
	if m.selID != "" {
		for i, wk := range rows {
			if wk.ID == m.selID {
				return i
			}
		}
	}
	return m.selected
}

// selectRow selects rows[i] and pins its id.
func (m *Model) selectRow(rows []workerRow, i int) {
	if i < 0 || i >= len(rows) {
		return
	}
	m.selected = i
	m.selID = rows[i].ID
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

// workerKeys reports whether the worker-table keys (↑↓/"", x, r, R, u)
// are live: on the dashboard, or on the Agents screen with the worker
// pane focused.
func (m Model) workerKeys() bool {
	return m.tab == tabDashboard || (m.tab == tabChat && m.focus != focusSup)
}

// switchTab moves to tab t. Leaving Worktrees closes the branch review;
// entering it loads the branches (the review owns the keyboard while the
// tab is up). Entering Analytics reloads the scorecard.
func (m *Model) switchTab(t int) {
	if t < 0 || t >= numTabs {
		return
	}
	if m.tab == tabWorktrees && t != tabWorktrees {
		m.reviewing = false
		m.diffText = ""
		m.diffScroll = 0
		m.brConfirm = ""
	}
	m.tab = t
	switch t {
	case tabWorktrees:
		m.branches = nil
		if m.OnListBranches != nil {
			m.branches = m.OnListBranches()
		}
		m.brIdx = 0
		m.diffText = ""
		m.brConfirm = ""
		m.reviewing = true
	case tabAnalytics:
		if m.OnScorecard != nil {
			m.scores = m.OnScorecard()
		}
	}
}

// cyclePane moves the Agents-screen focus to the next pane: supervisor →
// each active worker tile in spawn order → the bottom panel → back to the
// supervisor.
func (m *Model) cyclePane() {
	if m.focus == focusBottom {
		m.focus = focusSup
		return
	}
	active := m.activeWorkers()
	if m.hideTiles {
		active = nil // hidden tiles aren't focus stops
	}
	next := 0 // index into active of the tile to focus
	if m.focus == focusWork {
		next = len(active) // past the last tile: on to the bottom panel
		if sel := m.selectedWorker(); sel != nil {
			for i, wk := range active {
				if wk.ID == sel.ID {
					next = i + 1
					break
				}
			}
		}
	}
	if next >= len(active) {
		m.focus = focusBottom
		return
	}
	m.focus = focusWork
	rows := m.visibleWorkers()
	for i, wk := range rows {
		if wk.ID == active[next].ID {
			m.selectRow(rows, i)
			return
		}
	}
}

func (m Model) updateKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Typing only while the supervisor pane owns the keyboard: with the
	// worker focus active, plain keys drive the worker table instead of
	// the chat input.
	typing := m.tab == tabChat && m.focus == focusSup

	// The session picker owns the keyboard while open.
	if m.picking {
		switch msg.String() {
		case "esc", "q", "s":
			m.picking = false
		case "n":
			m.picking = false
			return m, func() tea.Msg { return NewSessionMsg{} }
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

	// The branch review overlay owns the keyboard while open.
	if m.reviewing {
		if m.diffText != "" {
			// Diff pane: close or scroll only.
			switch msg.String() {
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			case "esc", "q", "enter":
				m.diffText = ""
				m.diffScroll = 0
			case "down", "j":
				m.diffScroll++
			case "up", "k":
				if m.diffScroll > 0 {
					m.diffScroll--
				}
			case "pgdown":
				m.diffScroll += 20
			case "pgup":
				m.diffScroll -= 20
				if m.diffScroll < 0 {
					m.diffScroll = 0
				}
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "esc", "q", "b":
			m.switchTab(tabChat)
		case "1", "2", "3", "4", "5", "6":
			m.switchTab(int(msg.String()[0] - '1'))
		case "tab":
			m.switchTab((m.tab + 1) % numTabs)
		case "shift+tab":
			m.switchTab((m.tab + numTabs - 1) % numTabs)
		case "up", "k":
			if m.brIdx > 0 {
				m.brIdx--
			}
			m.brConfirm = ""
		case "down", "j":
			if m.brIdx < len(m.branches)-1 {
				m.brIdx++
			}
			m.brConfirm = ""
		case "enter":
			if m.brIdx < len(m.branches) && m.OnBranchDiff != nil {
				text, err := m.OnBranchDiff(m.branches[m.brIdx].Name)
				if err != nil {
					m.showToast("diff: " + err.Error())
					break
				}
				if text == "" {
					text = "(no changes)"
				}
				m.diffText = text
				m.diffScroll = 0
			}
		case "r":
			if m.OnListBranches != nil {
				m.branches = m.OnListBranches()
				if m.brIdx >= len(m.branches) {
					m.brIdx = len(m.branches) - 1
				}
				if m.brIdx < 0 {
					m.brIdx = 0
				}
				m.brConfirm = ""
			}
		case "m", "x":
			if m.brIdx >= len(m.branches) {
				break
			}
			name := m.branches[m.brIdx].Name
			kind := msg.String()
			key := kind + ":" + name
			if m.brConfirm != key {
				m.brConfirm = key
				if kind == "m" {
					m.showToast("press m again to merge " + name + " into your branch")
				} else {
					m.showToast("press x again to discard " + name + " — its work is deleted")
				}
				break
			}
			m.brConfirm = ""
			if kind == "m" {
				if m.OnMergeBranch == nil {
					m.showToast("demo replay — branch merge isn't wired")
					break
				}
				if err := m.OnMergeBranch(name); err != nil {
					m.showToast("merge: " + err.Error())
					break
				}
				m.showToast("merged " + name)
			} else {
				if m.OnDiscardBranch == nil {
					m.showToast("demo replay — branch discard isn't wired")
					break
				}
				if err := m.OnDiscardBranch(name); err != nil {
					m.showToast("discard: " + err.Error())
					break
				}
				m.showToast("discarded " + name)
			}
			// A successful merge/discard changes the list: reload it as
			// "r" does.
			if m.OnListBranches != nil {
				m.branches = m.OnListBranches()
				if m.brIdx >= len(m.branches) {
					m.brIdx = len(m.branches) - 1
				}
				if m.brIdx < 0 {
					m.brIdx = 0
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
			m.selected, m.selID = 0, ""
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
		if typing {
			if done, next := m.completeSlash(); done {
				return next, nil
			}
		}
		m.switchTab((m.tab + 1) % numTabs)
		return m, nil
	case "shift+tab":
		if m.tab == tabChat {
			m.cyclePane()
			return m, nil
		}
		m.switchTab((m.tab + numTabs - 1) % numTabs)
		return m, nil
	case "ctrl+t":
		// Hide/show the worker tiles, from either focus. A focused tile
		// that disappears hands the keyboard back to the supervisor.
		if m.tab == tabChat {
			m.hideTiles = !m.hideTiles
			if m.hideTiles && m.focus == focusWork {
				m.focus = focusSup
				m.zoomed = false
			}
		}
		return m, nil
	case "ctrl+o":
		// Zoom the focused Agents pane from either focus — checked
		// before the typing branch, so it works while typing.
		if m.tab == tabChat {
			m.zoomed = !m.zoomed
		}
		return m, nil
	case "esc":
		if m.tab == tabChat {
			if m.focus == focusSup {
				return m, func() tea.Msg { return InterruptMsg{} }
			}
			// Worker focus unwinds in order: zoom, then filter, then
			// back to the supervisor pane.
			if m.zoomed {
				m.zoomed = false
				return m, nil
			}
			if m.filter != "" {
				m.filter = ""
				m.filterInput.SetValue("")
				m.selected, m.selID = 0, ""
				return m, nil
			}
			m.focus = focusSup
			return m, nil
		}
		if m.tab == tabDashboard && m.filter != "" {
			m.filter = ""
			m.filterInput.SetValue("")
			m.selected, m.selID = 0, ""
			return m, nil
		}
		m.switchTab(tabChat)
		return m, nil
	}

	if typing {
		switch msg.String() {
		case "ctrl+v", "alt+v":
			if m.OnPasteImage == nil {
				m.showToast("demo replay — no clipboard")
				return m, nil
			}
			// The clipboard read happens off the Update loop; the result
			// arrives as an ImagePastedMsg.
			paste, key := m.OnPasteImage, msg.String()
			return m, func() tea.Msg {
				img, err := paste()
				return ImagePastedMsg{Image: img, Err: err, Key: key}
			}
		case "backspace":
			if m.input.Value() == "" && len(m.pending) > 0 {
				m.pending = m.pending[:len(m.pending)-1]
				m.showToast("image removed")
				return m, nil
			}
		case "enter":
			text := normalizeDroppedPaths(strings.TrimSpace(m.input.Value()))
			if text == "" && len(m.pending) == 0 {
				return m, nil
			}
			m.input.SetValue("")
			m.chatScroll = 0 // sending re-joins the live tail
			if text == "/new" {
				// /new starts a fresh session; pending images are kept.
				return m, func() tea.Msg { return NewSessionMsg{} }
			}
			images := m.pending
			m.pending = nil
			prefix := ""
			for _, img := range images {
				prefix += fmt.Sprintf("[image %d×%d] ", img.Width, img.Height)
			}
			m.chat = append(m.chat, chatItem{kind: "user", when: time.Now(),
				text: prefix + text})
			return m, func() tea.Msg { return SendPromptMsg{Text: text, Images: images} }
		case "pgup":
			m.chatScroll += m.chatPageStep()
			return m, nil
		case "pgdown":
			m.chatScroll -= m.chatPageStep()
			if m.chatScroll < 0 {
				m.chatScroll = 0
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	// non-typing tabs
	// Any key other than "u" disarms an armed undo confirmation.
	if msg.String() != "u" {
		m.undoConfirm = ""
	}
	switch msg.String() {
	case "q":
		m.quitting = true
		return m, tea.Quit
	case "1", "2", "3", "4", "5", "6":
		m.switchTab(int(msg.String()[0] - '1'))
	case "up", "k":
		if rows := m.visibleWorkers(); m.workerKeys() && m.selIndex(rows) > 0 {
			m.selectRow(rows, m.selIndex(rows)-1)
		}
	case "down", "j":
		if rows := m.visibleWorkers(); m.workerKeys() && m.selIndex(rows) < len(rows)-1 {
			m.selectRow(rows, m.selIndex(rows)+1)
		}
	case "/":
		if m.workerKeys() {
			m.filtering = true
			m.filterInput.SetValue(m.filter)
			m.filterInput.Focus()
		}
	case "z":
		if m.tab == tabChat && m.focus != focusSup {
			m.zoomed = !m.zoomed
		}
	case "t":
		if m.tab == tabChat && m.focus != focusSup {
			m.bottomTab = (m.bottomTab + 1) % numBottomTabs
		}
	case "right", "l":
		if m.tab == tabChat && m.focus == focusBottom {
			m.bottomTab = (m.bottomTab + 1) % numBottomTabs
		}
	case "left", "h":
		if m.tab == tabChat && m.focus == focusBottom {
			m.bottomTab = (m.bottomTab + numBottomTabs - 1) % numBottomTabs
		}
	case "s":
		if m.OnListSessions != nil {
			m.sessions = m.OnListSessions()
			m.sessIdx = 0
			m.picking = true
		} else {
			m.showToast("demo replay — no session history")
		}
	case "b":
		m.switchTab(tabWorktrees)
	case "n":
		return m, func() tea.Msg { return NewSessionMsg{} }
	case "f":
		if m.tab == tabLogs ||
			(m.tab == tabChat && m.focus != focusSup && m.bottomTab == bottomLogs) {
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
		if !m.workerKeys() {
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
		if !m.workerKeys() {
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
		if !m.workerKeys() {
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
	case "u":
		if !m.workerKeys() {
			break
		}
		wk := m.selectedWorker()
		if wk == nil {
			break
		}
		if wk.Status != "done" && wk.Status != "failed" {
			m.showToast(wk.ID + " is still running — nothing to undo yet")
			break
		}
		if m.OnUndoWorker == nil {
			m.showToast("demo replay — no undo")
			break
		}
		if m.undoConfirm != wk.ID {
			m.undoConfirm = wk.ID
			m.showToast("press u again to revert " + wk.ID + "'s edits")
			break
		}
		m.undoConfirm = ""
		id, dir := wk.ID, wk.Dir
		files, err := m.OnUndoWorker(id, dir)
		if err != nil {
			m.showToast("undo " + id + ": " + err.Error())
			break
		}
		if len(files) == 0 {
			m.showToast(id + " changed no files")
			break
		}
		m.showToast(fmt.Sprintf("reverted %d file(s) from %s", len(files), id))
	}
	return m, nil
}

// squeezeSpace collapses every run of whitespace (including newlines and
// tabs) in the first cap bytes of s to a single space.
func squeezeSpace(s string, cap int) string {
	if len(s) > cap {
		s = s[:cap]
	}
	return strings.Join(strings.Fields(s), " ")
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
// spawn last, like the mock's live rows), then finished newest first —
// the row that just ended sits right under the live ones, and the
// history a session has replayed from the registry trails off below.
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
	finishedAt := func(w workerRow) time.Time {
		if !w.Ended.IsZero() {
			return w.Ended
		}
		return w.Started
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i]), rank(out[j])
		if ri != rj {
			return ri < rj
		}
		if ri < 2 {
			return false // live rows keep arrival order
		}
		ti, tj := finishedAt(out[i]), finishedAt(out[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return workerNum(out[i].ID) > workerNum(out[j].ID)
	})
	return out
}

// workerNum is the numeric part of a "wN" id (0 when it has none), so
// same-instant rows still order newest first.
func workerNum(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "w"))
	return n
}
