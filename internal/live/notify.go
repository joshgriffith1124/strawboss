package live

import (
	"net/http"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// Remote reach lives here: alerts fan out to ntfy. All of it is local
// HTTP; nothing observes the supervisor by talking to it.

// notifyText sends one alert to every configured push channel.
func (o *Orchestrator) notifyText(title, text string) {
	o.pushNtfy(title, text)
}

// pushFailure alerts on a worker failure.
func (o *Orchestrator) pushFailure(worker, summary string) {
	o.notifyText("strawboss: worker failed", worker+" failed — "+summary)
}

// observeSup watches the mapped supervisor msgs: turn errors alert.
func (o *Orchestrator) observeSup(msgs []tea.Msg) {
	for _, m := range msgs {
		switch v := m.(type) {
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
			// Silent auto-denials get a push too, once per suggestion —
			// while away, a dead-ended supervisor is exactly what ntfy
			// exists to surface.
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

func truncN(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
