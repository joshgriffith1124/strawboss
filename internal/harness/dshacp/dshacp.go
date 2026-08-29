// Package dshacp implements harness.WorkerHarness over the DeepSeek
// Harness (dsh) ACP automation server: one `dsh-acp-demo` subprocess per
// worker, driven by JSON-RPC over stdio (ndjson). The worker id is the ACP
// session id. Behavior and setup requirements: docs/NOTES.md
// ("DeepSeek Harness (dsh) 0.1.1-rc.2").
package dshacp

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/harness"
)

//go:embed cordis.yml
var cordisTemplate []byte

// Harness runs dsh workers. Fields left zero take defaults resolved at
// Spawn time.
type Harness struct {
	// ProfileDir is the dsh acp profile the bin and plugins resolve from.
	// Default: $DSH_HOME/profiles/acp, else ~/.dsh/profiles/acp.
	ProfileDir string
	// Bin overrides the dsh-acp-demo path (default: under ProfileDir).
	Bin string
	// Config overrides the cordis.yml path (default:
	// <ProfileDir>/strawboss.cordis.yml, generated from the embedded
	// template when missing — never overwritten).
	Config string
	// Dir is the working directory workers run in.
	Dir string
	// LogDir is where Result writes full transcripts.
	LogDir string
	// SessionsRoot is the dsh persistence root (session JSONLs).
	SessionsRoot string
	// BootTimeout bounds initialize + session/new. Default 60s.
	BootTimeout time.Duration

	mu      sync.Mutex
	workers map[string]*worker
}

// New returns a Harness rooted in the state dir layout delegate and the
// TUI share: logs under <stateDir>/logs, session JSONLs under
// <stateDir>/dsh-sessions.
func New(dir, stateDir string) *Harness {
	return &Harness{
		Dir:          dir,
		LogDir:       filepath.Join(stateDir, "logs"),
		SessionsRoot: filepath.Join(stateDir, "dsh-sessions"),
	}
}

// DefaultProfileDir resolves the dsh acp profile location.
func DefaultProfileDir() string {
	home := os.Getenv("DSH_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".dsh")
		}
	}
	return filepath.Join(home, "profiles", "acp")
}

// SessionsRootFor is the dsh persistence root under a state dir — the TUI
// tails session logs from here.
func SessionsRootFor(stateDir string) string {
	return filepath.Join(stateDir, "dsh-sessions")
}

// Field overrides win, then STRAWBOSS_DSH_{PROFILE,BIN,CONFIG} env vars
// (handy for tests and non-standard installs), then defaults.

func (h *Harness) profileDir() string {
	if h.ProfileDir != "" {
		return h.ProfileDir
	}
	if v := os.Getenv("STRAWBOSS_DSH_PROFILE"); v != "" {
		return v
	}
	return DefaultProfileDir()
}

// ensureConfig writes the embedded worker composition next to the profile
// if none exists yet. An existing file is the user's to tune.
func (h *Harness) ensureConfig() (string, error) {
	path := h.Config
	if path == "" {
		path = os.Getenv("STRAWBOSS_DSH_CONFIG")
	}
	if path == "" {
		path = filepath.Join(h.profileDir(), "strawboss.cordis.yml")
	}
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if _, err := os.Stat(h.profileDir()); err != nil {
		return "", fmt.Errorf("dsh acp profile not found at %s — install dsh and the acp profile packages (docs/NOTES.md): %w", h.profileDir(), err)
	}
	if err := os.WriteFile(path, cordisTemplate, 0o644); err != nil {
		return "", fmt.Errorf("writing dsh worker config: %w", err)
	}
	return path, nil
}

func (h *Harness) bin() string {
	if h.Bin != "" {
		return h.Bin
	}
	if v := os.Getenv("STRAWBOSS_DSH_BIN"); v != "" {
		return v
	}
	return filepath.Join(h.profileDir(), "node_modules", ".bin", "dsh-acp-demo")
}

// worker is one dsh subprocess + its ACP conversation.
type worker struct {
	cmd   *exec.Cmd
	stdin interface {
		Write([]byte) (int, error)
		Close() error
	}
	stderr *ringBuffer
	proxy  *llmProxy

	mu       sync.Mutex
	nextID   int
	pending  map[int]chan rpcReply
	chunks   []string // committed agent_message_chunk texts
	promptID int
	stop     string // stopReason once the prompt settled
	failErr  string // rpc/process error once settled
	settled  chan struct{}
	done     bool
}

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Spawn boots one dsh-acp-demo, creates a session in h.Dir, and fires the
// task at it without waiting. The model config supplies the LLM endpoint
// (an OpenAI-compatible base URL) and wire model id via env, which the
// generated cordis.yml reads.
func (h *Harness) Spawn(ctx context.Context, task string, mc config.ModelConfig) (string, error) {
	cfgPath, err := h.ensureConfig()
	if err != nil {
		return "", fmt.Errorf("spawning dsh worker: %w", err)
	}
	bin := h.bin()
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("spawning dsh worker: dsh-acp-demo bin not found at %s (docs/NOTES.md): %w", bin, err)
	}
	// Each worker gets its OWN persistence subtree: the acp app derives a
	// session-query.db (SQLite, single-writer) at the persistence root,
	// and concurrent workers sharing one root die at boot with
	// "ERR_SQLITE_ERROR: database is locked". FindSessionLog globs across
	// the extra level.
	sub := fmt.Sprintf("w-%d-%s", os.Getpid(), strconv.FormatInt(time.Now().UnixNano()%1e9, 36))
	sessionsAbs, err := filepath.Abs(filepath.Join(h.SessionsRoot, sub))
	if err != nil {
		return "", fmt.Errorf("spawning dsh worker: %w", err)
	}
	if err := os.MkdirAll(sessionsAbs, 0o755); err != nil {
		return "", fmt.Errorf("spawning dsh worker: %w", err)
	}
	apiKey := mc.APIKey
	if apiKey == "" {
		apiKey = "local"
	}
	// The worker's LLM traffic goes through a local translating proxy
	// (see proxy.go: sglang's null-bearing tool_calls deltas).
	proxy, err := startLLMProxy(mc.Endpoint)
	if err != nil {
		return "", fmt.Errorf("spawning dsh worker: %w", err)
	}

	cmd := exec.Command(bin, "--config", cfgPath)
	cmd.Dir = h.Dir
	cmd.Env = append(os.Environ(),
		"STRAWBOSS_LLM_BASE_URL="+proxy.URL(),
		"STRAWBOSS_LLM_API_KEY="+apiKey,
		"STRAWBOSS_DSH_MODEL="+mc.Model,
		"STRAWBOSS_DSH_SESSIONS="+sessionsAbs,
	)
	if mc.ToolsMode != "" {
		cmd.Env = append(cmd.Env, "STRAWBOSS_DSH_TOOLS_MODE="+mc.ToolsMode)
	}
	maxTokens := mc.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 49152 // opencode-limit parity; see config.ModelConfig
	}
	cmd.Env = append(cmd.Env, "STRAWBOSS_DSH_MAXTOKENS="+strconv.Itoa(maxTokens))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		proxy.Close()
		return "", fmt.Errorf("spawning dsh worker: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		proxy.Close()
		return "", fmt.Errorf("spawning dsh worker: %w", err)
	}
	w := &worker{
		cmd:     cmd,
		stdin:   stdin,
		stderr:  newRingBuffer(4096),
		proxy:   proxy,
		pending: map[int]chan rpcReply{},
		settled: make(chan struct{}),
	}
	cmd.Stderr = w.stderr
	if err := cmd.Start(); err != nil {
		proxy.Close()
		return "", fmt.Errorf("spawning dsh worker: %w", err)
	}
	go w.readLoop(stdout)
	go func() { _ = cmd.Wait() }() // reap; ProcessState marks death

	boot := h.BootTimeout
	if boot == 0 {
		boot = 60 * time.Second
	}
	bootCtx, cancel := context.WithTimeout(ctx, boot)
	defer cancel()

	fail := func(err error) (string, error) {
		w.shutdown()
		return "", fmt.Errorf("spawning dsh worker: %w%s", err, w.stderrHint())
	}
	if _, err := w.call(bootCtx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	}); err != nil {
		return fail(err)
	}
	dirAbs, err := filepath.Abs(h.Dir)
	if err != nil {
		return fail(err)
	}
	raw, err := w.call(bootCtx, "session/new", map[string]any{"cwd": dirAbs, "mcpServers": []any{}})
	if err != nil {
		return fail(err)
	}
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &sess); err != nil || sess.SessionID == "" {
		return fail(fmt.Errorf("session/new returned no sessionId: %s", raw))
	}
	if err := w.prompt(sess.SessionID, task); err != nil {
		return fail(err)
	}

	h.mu.Lock()
	if h.workers == nil {
		h.workers = map[string]*worker{}
	}
	h.workers[sess.SessionID] = w
	h.mu.Unlock()
	return sess.SessionID, nil
}

func (h *Harness) worker(id string) (*worker, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.workers[id]
	if w == nil {
		return nil, fmt.Errorf("worker %s: not spawned by this process", id)
	}
	return w, nil
}

// Status reports the in-process view: running until the prompt settles.
func (h *Harness) Status(ctx context.Context, workerID string) (harness.Status, error) {
	w, err := h.worker(workerID)
	if err != nil {
		return "", err
	}
	select {
	case <-w.settled:
	default:
		return harness.StatusRunning, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop == "end_turn" {
		return harness.StatusDone, nil
	}
	return harness.StatusFailed, nil
}

// Events streams the worker's transcript by tailing its session log.
func (h *Harness) Events(ctx context.Context, workerID string) (<-chan harness.Event, error) {
	if _, err := h.worker(workerID); err != nil {
		return nil, err
	}
	items := TailSession(ctx, h.SessionsRoot, workerID, 0)
	out := make(chan harness.Event, 64)
	go func() {
		defer close(out)
		for it := range items {
			if it.Event == nil {
				continue
			}
			select {
			case out <- *it.Event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Usage reads cumulative token counts from the session log.
func (h *Harness) Usage(ctx context.Context, workerID string) (harness.Usage, error) {
	path, err := FindSessionLog(h.SessionsRoot, workerID)
	if err != nil {
		return harness.Usage{}, nil // no log yet — zero, not an error
	}
	info, err := ReadSession(path)
	if err != nil {
		return harness.Usage{}, fmt.Errorf("worker %s usage: %w", workerID, err)
	}
	return info.Usage, nil
}

// maxSummaryBytes caps the terse summary (CLAUDE.md invariant 3).
const maxSummaryBytes = 700

// Result blocks until the prompt settles and returns the terse result. A
// cancelled ctx aborts the worker and reports it failed.
func (h *Harness) Result(ctx context.Context, workerID string) (harness.Result, error) {
	w, err := h.worker(workerID)
	if err != nil {
		return harness.Result{}, err
	}
	aborted := false
	select {
	case <-w.settled:
	case <-ctx.Done():
		aborted = true
		_ = h.Abort(context.Background(), workerID)
	}
	defer w.shutdown()

	var info SessionInfo
	logPath := ""
	if src, err := FindSessionLog(h.SessionsRoot, workerID); err == nil {
		info, _ = ReadSession(src)
		logPath, err = h.copyLog(workerID, src)
		if err != nil {
			return harness.Result{}, err
		}
	}

	w.mu.Lock()
	stop, failErr := w.stop, w.failErr
	summary := strings.TrimSpace(strings.Join(w.chunks, ""))
	w.mu.Unlock()

	status := harness.StatusDone
	switch {
	case aborted:
		status = harness.StatusFailed
		summary = strings.TrimSpace(fmt.Sprintf("aborted: %v. %s", context.Cause(ctx), summary))
	case failErr != "":
		status = harness.StatusFailed
		summary = strings.TrimSpace(failErr + w.stderrHint() + "\n" + summary)
	case stop != "end_turn":
		status = harness.StatusFailed
		summary = strings.TrimSpace("worker stopped without finishing (stopReason " + stop + "). " + summary)
	}
	if summary == "" {
		summary = strings.TrimSpace(info.LastText)
	}
	if status == harness.StatusDone && summary == "" {
		// No committed answer at all: on small local models this is the
		// output-budget-exhausted signature — fail with advice rather
		// than let the supervisor retry the same task forever.
		status = harness.StatusFailed
		summary = "worker produced no answer (model finish: " + info.FinishReason +
			") — likely output budget exhausted. Do NOT retry the same task: split it into smaller pieces or demand a much smaller deliverable."
	}
	if len(summary) > maxSummaryBytes {
		summary = summary[:maxSummaryBytes] + "…"
	}
	return harness.Result{WorkerID: workerID, Status: status, Summary: summary, LogPath: logPath}, nil
}

// Abort cancels the worker's prompt (ACP session/cancel), waiting briefly
// for settlement before shutting the process down.
func (h *Harness) Abort(ctx context.Context, workerID string) error {
	w, err := h.worker(workerID)
	if err != nil {
		return err
	}
	_ = w.notify("session/cancel", map[string]any{"sessionId": workerID})
	select {
	case <-w.settled:
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
	}
	w.shutdown()
	return nil
}

// PID exposes the worker subprocess pid (recorded in the registry so the
// TUI can kill a worker owned by another delegate process).
func (h *Harness) PID(workerID string) int {
	w, err := h.worker(workerID)
	if err != nil || w.cmd.Process == nil {
		return 0
	}
	return w.cmd.Process.Pid
}

func (h *Harness) copyLog(workerID, src string) (string, error) {
	if err := os.MkdirAll(h.LogDir, 0o755); err != nil {
		return "", fmt.Errorf("worker %s log: %w", workerID, err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("worker %s log: %w", workerID, err)
	}
	dst := filepath.Join(h.LogDir, workerID+".jsonl")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", fmt.Errorf("worker %s log: %w", workerID, err)
	}
	return dst, nil
}

// ── worker: JSON-RPC over the subprocess stdio ─────────────────────────

// call sends a request and waits for its reply.
func (w *worker) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		return nil, fmt.Errorf("%s: process exited", method)
	}
	w.nextID++
	id := w.nextID
	ch := make(chan rpcReply, 1)
	w.pending[id] = ch
	w.mu.Unlock()

	if err := w.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	select {
	case reply := <-ch:
		if reply.Error != nil {
			return nil, fmt.Errorf("%s: %s (%d)", method, reply.Error.Message, reply.Error.Code)
		}
		return reply.Result, nil
	case <-w.settled:
		return nil, fmt.Errorf("%s: process exited before replying", method)
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// prompt fires session/prompt; its reply settles the worker.
func (w *worker) prompt(sessionID, task string) error {
	w.mu.Lock()
	w.nextID++
	w.promptID = w.nextID
	id := w.nextID
	w.mu.Unlock()
	return w.send(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt":    []map[string]any{{"type": "text", "text": task}},
		},
	})
}

func (w *worker) notify(method string, params any) error {
	return w.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (w *worker) send(msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = w.stdin.Write(append(b, '\n'))
	return err
}

// readLoop parses ndjson frames off stdout until EOF.
func (w *worker) readLoop(r interface{ Read([]byte) (int, error) }) {
	defer w.settle("", "worker process exited unexpectedly")
	buf := make([]byte, 0, 64<<10)
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for {
			nl := -1
			for i, c := range buf {
				if c == '\n' {
					nl = i
					break
				}
			}
			if nl < 0 {
				break
			}
			line := buf[:nl]
			buf = append([]byte{}, buf[nl+1:]...)
			w.handleLine(line)
		}
		if err != nil {
			return
		}
	}
}

func (w *worker) handleLine(line []byte) {
	var msg struct {
		ID     *int            `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		rpcReply
	}
	if json.Unmarshal(line, &msg) != nil {
		return
	}
	switch {
	case msg.Method == "session/update":
		var p struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		if json.Unmarshal(msg.Params, &p) == nil && p.Update.SessionUpdate == "agent_message_chunk" && p.Update.Content.Type == "text" {
			w.mu.Lock()
			w.chunks = append(w.chunks, p.Update.Content.Text)
			w.mu.Unlock()
		}
	case msg.Method == "session/request_permission" && msg.ID != nil:
		// Policy should prevent these entirely (docs/NOTES.md); if one
		// arrives anyway, answer with the first allow-ish option so the
		// worker can never block (CLAUDE.md invariant 6's analog).
		var p struct {
			Options []struct {
				OptionID string `json:"optionId"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		optionID := ""
		if json.Unmarshal(msg.Params, &p) == nil && len(p.Options) > 0 {
			optionID = p.Options[0].OptionID
			for _, o := range p.Options {
				if strings.Contains(o.Kind, "allow") {
					optionID = o.OptionID
					break
				}
			}
		}
		_ = w.send(map[string]any{"jsonrpc": "2.0", "id": *msg.ID,
			"result": map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optionID}}})
	case msg.ID != nil && msg.Method == "":
		w.mu.Lock()
		if *msg.ID == w.promptID && w.promptID != 0 {
			w.mu.Unlock()
			if msg.Error != nil {
				w.settle("", msg.Error.Message)
				return
			}
			var res struct {
				StopReason string `json:"stopReason"`
			}
			_ = json.Unmarshal(msg.Result, &res)
			w.settle(res.StopReason, "")
			return
		}
		ch := w.pending[*msg.ID]
		delete(w.pending, *msg.ID)
		w.mu.Unlock()
		if ch != nil {
			ch <- msg.rpcReply
		}
	}
}

// settle records the prompt outcome exactly once.
func (w *worker) settle(stop, failErr string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return
	}
	w.done = true
	w.stop, w.failErr = stop, failErr
	close(w.settled)
}

// shutdown closes stdin (graceful: dsh flushes sessions on EOF) and kills
// the process if it lingers.
func (w *worker) shutdown() {
	if w.proxy != nil {
		defer w.proxy.Close()
	}
	_ = w.stdin.Close()
	if w.cmd.Process == nil {
		return
	}
	for i := 0; i < 60; i++ {
		if w.cmd.ProcessState != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = w.cmd.Process.Signal(syscall.SIGKILL)
}

func (w *worker) stderrHint() string {
	tail := strings.TrimSpace(w.stderr.String())
	if tail == "" {
		return ""
	}
	if len(tail) > 500 {
		tail = "…" + tail[len(tail)-500:]
	}
	return " (stderr: " + tail + ")"
}

// ringBuffer keeps the last n bytes written (stderr diagnostics).
type ringBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newRingBuffer(max int) *ringBuffer { return &ringBuffer{max: max} }

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

var _ harness.WorkerHarness = (*Harness)(nil)
