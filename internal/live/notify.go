package live

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// Remote reach lives here: alerts fan out to ntfy and/or an OpenClaw
// channel (Discord etc.), and with two-way enabled the OpenClaw channel
// is polled so the user can steer a stuck operation from a phone — replies
// are injected as ordinary supervisor prompts (mid-turn input works) and
// the supervisor's answers are relayed back. All of it is local HTTP or
// CLI execs; nothing observes the supervisor by talking to it.

// notifyText sends one alert to every configured push channel.
func (o *Orchestrator) notifyText(title, text string) {
	o.pushNtfy(title, text)
	o.pushOpenClaw(text)
}

// pushFailure alerts on a worker failure.
func (o *Orchestrator) pushFailure(worker, summary string) {
	o.notifyText("strawboss: worker failed", worker+" failed — "+summary)
}

// observeSup watches the mapped supervisor msgs: turn errors alert, and
// while a remote conversation is active, completed replies relay back to
// the OpenClaw channel so the remote user sees the outcome.
func (o *Orchestrator) observeSup(msgs []tea.Msg) {
	for _, m := range msgs {
		switch v := m.(type) {
		case ui.SupTextDoneMsg:
			if o.remoteActive.Load() && strings.TrimSpace(v.Text) != "" {
				o.pushOpenClaw(v.Text)
			}
		case ui.SupTurnDoneMsg:
			if v.Err != "" {
				o.notifyText("strawboss: supervisor error", "supervisor error — "+truncN(v.Err, 300))
			}
		case ui.SupToolMsg:
			o.mu.Lock()
			o.toolCmds[v.ToolID] = v.Name + " " + v.Command
			if len(o.toolCmds) > 100 {
				o.toolCmds = map[string]string{v.ToolID: v.Name + " " + v.Command}
			}
			o.mu.Unlock()
		case ui.SupToolResultMsg:
			// Silent auto-denials get a remote notice too, once per
			// suggestion — while away, a dead-ended supervisor is exactly
			// what the Discord channel exists to surface.
			if tool := ui.DeniedTool(v.Content); tool != "" {
				o.mu.Lock()
				cmd := o.toolCmds[v.ToolID]
				sug := ui.AllowSuggestion(tool, cmd)
				dup := o.deniedNotified[sug]
				o.deniedNotified[sug] = true
				o.mu.Unlock()
				if !dup {
					o.notifyText("strawboss: supervisor denied "+tool,
						"supervisor was denied "+truncN(cmd, 120)+" — allow with "+sug+" in supervisor.allowed_tools")
				}
			}
		case ui.SupTurnUsageMsg:
			o.noteSupCtx(v.Input + v.CacheRead + v.CacheWrite)
		case ui.SupUsageMsg:
			o.recordSupUsage(v)
			o.noteBudgetUsage(v.CostUSD, 0)
		case ui.SupRateLimitMsg:
			o.noteBudgetUsage(0, v.FiveHour)
		}
	}
}

func (o *Orchestrator) pushNtfy(title, text string) {
	topic := o.Notify.NtfyTopic
	if topic == "" {
		return
	}
	server := o.Notify.NtfyServer
	if server == "" {
		server = "https://ntfy.sh"
	}
	url := strings.TrimRight(server, "/") + "/" + topic
	go func() {
		req, err := http.NewRequest("POST", url, strings.NewReader(text))
		if err != nil {
			o.emitAsync(ui.RawLogMsg{Source: "app", Line: "ntfy: " + err.Error()})
			return
		}
		req.Header.Set("Title", title)
		req.Header.Set("Priority", "high")
		req.Header.Set("Tags", "rotating_light")
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			o.emitAsync(ui.RawLogMsg{Source: "app", Line: "ntfy: " + err.Error()})
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			o.emitAsync(ui.RawLogMsg{Source: "app", Line: "ntfy: status " + resp.Status})
		}
	}()
}

// ── OpenClaw ───────────────────────────────────────────────────────────

// maxBackoff caps the openclawLoop poll backoff after consecutive failures.
const maxBackoff = 2 * time.Minute

func (o *Orchestrator) openclawBin() string {
	if o.Notify.OpenClawBin != "" {
		return o.Notify.OpenClawBin
	}
	return "openclaw"
}

func (o *Orchestrator) openclawChannel() string {
	if o.Notify.OpenClawChannel != "" {
		return o.Notify.OpenClawChannel
	}
	return "discord"
}

// openclawCmd builds an exec.Cmd for the openclaw CLI. The CLI is a Node
// program: every start writes files into /tmp/node-compile-cache (a tmpfs
// flood at a 5s poll), and it spawns children of its own that linger when
// only the top process is killed. So: NODE_DISABLE_COMPILE_CACHE=1 stops
// the cache writes, the child runs in its own process group so Cancel can
// SIGKILL the whole group (children included) on timeout, and WaitDelay
// keeps Output/CombinedOutput from hanging on pipes a grandchild still
// holds. Used for both the poll reads and message sends.
func (o *Orchestrator) openclawCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, o.openclawBin(), args...)
	cmd.Env = append(os.Environ(), "NODE_DISABLE_COMPILE_CACHE=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// pushOpenClaw sends one message to the configured channel target.
func (o *Orchestrator) pushOpenClaw(text string) {
	target := o.Notify.OpenClawTarget
	if target == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := o.openclawCmd(ctx, "message", "send",
			"--channel", o.openclawChannel(), "--target", target,
			"--message", truncN(text, 1800), "--json")
		if out, err := cmd.CombinedOutput(); err != nil {
			o.emitAsync(ui.RawLogMsg{Source: "app",
				Line: "openclaw send: " + err.Error() + ": " + truncN(strings.TrimSpace(string(out)), 200)})
		}
	}()
}

// openclawMessage is the slice of the Discord-shaped read payload we use.
type openclawMessage struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Author  struct {
		Bot      bool   `json:"bot"`
		Username string `json:"username"`
	} `json:"author"`
}

func (o *Orchestrator) openclawRead(ctx context.Context, afterID uint64) ([]openclawMessage, error) {
	args := []string{"message", "read", "--channel", o.openclawChannel(),
		"--target", o.Notify.OpenClawTarget, "--limit", "25", "--json"}
	if afterID > 0 {
		args = append(args, "--after", strconv.FormatUint(afterID, 10))
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := o.openclawCmd(ctx, args...).Output()
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Payload struct {
			Messages []openclawMessage `json:"messages"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, err
	}
	return parsed.Payload.Messages, nil
}

// openclawLoop polls the channel and turns the user's messages into
// supervisor prompts. History from before startup is baseline, never
// commands; bot-authored messages (our own notifications, the OpenClaw
// agent) are ignored, so nothing can feed back.
func (o *Orchestrator) openclawLoop(ctx context.Context) {
	if o.Notify.OpenClawTarget == "" || !o.Notify.OpenClawTwoWay {
		return
	}
	o.emitAsync(ui.RemoteMsg{Channel: o.openclawChannel()})
	interval := o.OpenClawPollEvery
	if interval <= 0 {
		interval = 5 * time.Second
	}
	var lastID uint64
	baselined := false
	failing := false
	// Back off while reads fail: each consecutive failure doubles the wait
	// (capped at 2 minutes) so a dead channel doesn't spin; any success
	// resets to the normal interval. Only the first failure is logged.
	wait := interval
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		msgs, err := o.openclawRead(ctx, lastID)
		if err != nil {
			if !failing {
				failing = true
				o.emitAsync(ui.RawLogMsg{Source: "app", Line: "openclaw read: " + err.Error()})
			}
			if wait *= 2; wait > maxBackoff {
				wait = maxBackoff
			}
			continue
		}
		failing = false
		wait = interval
		ids := make([]uint64, 0, len(msgs))
		byID := map[uint64]openclawMessage{}
		for _, m := range msgs {
			id, err := strconv.ParseUint(m.ID, 10, 64)
			if err != nil {
				continue
			}
			ids = append(ids, id)
			byID[id] = m
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		if !baselined {
			baselined = true
			if len(ids) > 0 {
				lastID = ids[len(ids)-1]
			}
			continue
		}
		for _, id := range ids {
			if id <= lastID {
				continue
			}
			lastID = id
			m := byID[id]
			text := strings.TrimSpace(m.Content)
			if m.Author.Bot || text == "" {
				continue
			}
			o.remoteActive.Store(true)
			o.emitAsync(ui.SupUserMsg{Text: "[" + o.openclawChannel() + "] " + text, Time: time.Now()})
			o.enqueuePrompt(prompt{text: "[message from the user via " + o.openclawChannel() + " — they are away from the terminal; keep the reply brief] " + text})
		}
	}
}

func truncN(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
