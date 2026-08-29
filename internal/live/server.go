package live

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// ensureServers keeps `opencode serve` alive for every localhost endpoint
// in the model configs. A down local endpoint is spawned as a child
// process (logs under the state dir) and respawned if it dies; remote
// endpoints are only ever reported, never managed.
func (o *Orchestrator) ensureServers(ctx context.Context) {
	children := map[string]*exec.Cmd{} // endpoint base → running child
	check := func() {
		for _, base := range o.endpoints() {
			u, err := url.Parse(base)
			if err != nil || u.Port() == "" {
				continue
			}
			host := u.Hostname()
			if host != "127.0.0.1" && host != "localhost" && host != "::1" {
				continue
			}
			if healthy(ctx, base) {
				continue
			}
			if c := children[base]; c != nil && c.ProcessState == nil {
				continue // starting up or wedged; give it time
			}
			logPath := filepath.Join(o.StateDir, "opencode-serve.log")
			logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				o.emitAsync(ui.RawLogMsg{Source: "app", Line: "opencode serve log: " + err.Error()})
				continue
			}
			cmd := exec.Command("opencode", "serve", "--port", u.Port(), "--hostname", host)
			cmd.Dir = o.StateDir
			cmd.Stdout = logf
			cmd.Stderr = logf
			if err := cmd.Start(); err != nil {
				logf.Close()
				o.emitAsync(ui.RawLogMsg{Source: "app", Line: "spawning opencode serve: " + err.Error()})
				continue
			}
			logf.Close()
			children[base] = cmd
			o.mu.Lock()
			o.servers = append(o.servers, cmd)
			o.mu.Unlock()
			go func() { _ = cmd.Wait() }() // reap; ProcessState marks death
			o.emitAsync(ui.RawLogMsg{Source: "app",
				Line: "started opencode serve on " + base + " (log " + logPath + ")"})
		}
	}

	check()
	for {
		select {
		case <-ctx.Done():
			return // Shutdown owns killing the children
		case <-time.After(5 * time.Second):
			check()
		}
	}
}

func healthy(ctx context.Context, base string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/global/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
