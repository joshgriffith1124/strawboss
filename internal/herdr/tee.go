package herdr

import (
	"maps"

	tea "github.com/charmbracelet/bubbletea"
)

// Tee passes the feed through unchanged while mirroring it to herdr. The
// tracker runs inline (pure, cheap); socket writes happen on a separate
// goroutine that only ever sees the latest Status, so a slow or absent
// herdr server can never stall the UI's feed. stop clears the pane's
// tokens and releases the agent label; call it on exit.
func Tee(c *Client, in <-chan tea.Msg) (out <-chan tea.Msg, stop func()) {
	fwd := make(chan tea.Msg)
	latest := make(chan Status, 1)
	alerts := make(chan Alert, 16)
	quit := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(fwd)
		t := NewTracker()
		publish(latest, t.Status()) // show up in herdr right away
		for m := range in {
			for _, a := range t.Observe(m) {
				select {
				case alerts <- a:
				default: // a burst of failures: the bell and the TUI still have them
				}
			}
			publish(latest, t.Status())
			fwd <- m
		}
	}()

	go func() {
		defer close(finished)
		w := writer{c: c}
		for {
			select {
			case s := <-latest:
				w.report(s)
			case a := <-alerts:
				_ = c.Notify(a.Title, a.Body)
			case <-quit:
				w.release()
				return
			}
		}
	}()

	var stopped bool
	return fwd, func() {
		if stopped {
			return
		}
		stopped = true
		close(quit)
		<-finished
	}
}

// publish replaces whatever Status is pending with s.
func publish(ch chan Status, s Status) {
	select {
	case <-ch:
	default:
	}
	ch <- s
}

// writer sends only what changed since the last successful report.
type writer struct {
	c      *Client
	sent   bool
	state  string
	msg    string
	title  string
	tokens map[string]string
}

func (w *writer) report(s Status) {
	if !w.sent || s.State != w.state || s.Message != w.msg {
		if w.c.ReportState(s.State, s.Message) == nil {
			w.state, w.msg = s.State, s.Message
		}
	}
	if !w.sent || s.Title != w.title || !maps.Equal(s.Tokens, w.tokens) {
		if w.c.ReportMetadata(s.Title, tokenUpdate(s.Tokens)) == nil {
			w.title, w.tokens = s.Title, s.Tokens
		}
	}
	w.sent = true
}

func (w *writer) release() {
	_ = w.c.ReportMetadata("", tokenUpdate(nil))
	_ = w.c.Release()
}

// tokenUpdate is the full token set for a report: every current value,
// and nil for each known token that is now absent so herdr clears it.
func tokenUpdate(cur map[string]string) map[string]*string {
	out := make(map[string]*string, len(tokenNames))
	for _, name := range tokenNames {
		if v, ok := cur[name]; ok {
			out[name] = &v
		} else {
			out[name] = nil
		}
	}
	return out
}
