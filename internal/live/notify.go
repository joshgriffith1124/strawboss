package live

import (
	"net/http"
	"strings"
	"time"

	"strawboss/internal/ui"
)

// pushFailure sends a worker failure to the configured ntfy topic —
// fire-and-forget local HTTP; a failed push is a log line, never a crash,
// and none of this touches supervisor context.
func (o *Orchestrator) pushFailure(worker, summary string) {
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
		req, err := http.NewRequest("POST", url, strings.NewReader(worker+" failed — "+summary))
		if err != nil {
			o.emitAsync(ui.RawLogMsg{Source: "app", Line: "ntfy: " + err.Error()})
			return
		}
		req.Header.Set("Title", "strawboss: worker failed")
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
