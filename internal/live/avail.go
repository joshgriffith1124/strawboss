package live

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/config"
)

// Available reports whether a task sent to mc right now has somewhere to
// land, and when not, why — the same two conditions the dashboard shows
// ("endpoint unreachable", "model not loaded"). The delegate command asks
// before dispatching and before escalating so a task routes around a dead
// tier instead of burning a guaranteed failure on it (a fast model on a
// workstation comes and goes; the inference host serves one model at a
// time).
//
// It errs toward available: a reachable endpoint whose model list cannot be
// parsed counts, and so does an opencode server that answers at all — the
// probe must never be the reason a working model goes unused.
func Available(ctx context.Context, mc config.ModelConfig) (bool, string) {
	base := strings.TrimRight(mc.Endpoint, "/")
	if mc.Harness == "dsh" {
		models, ok := llmModels(ctx, base)
		if !ok {
			return false, "endpoint unreachable"
		}
		if models != nil {
			if _, served := models[mc.Model]; !served {
				return false, "model not loaded"
			}
		}
		return true, ""
	}
	// opencode: the endpoint is the local opencode server, which cannot say
	// cheaply whether its provider is up. Any HTTP answer means it is there.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/global/health", nil)
	if err != nil {
		return false, "endpoint unreachable"
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "endpoint unreachable"
	}
	resp.Body.Close()
	return true, ""
}
