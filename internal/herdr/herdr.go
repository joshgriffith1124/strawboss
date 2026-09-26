// Package herdr reports strawboss to herdr, the terminal workspace
// manager, when strawboss runs inside a herdr pane: the pane shows up in
// herdr's agent sidebar as "strawboss" with a working/idle state, custom
// sidebar tokens ($workers, $local_tok, $sup_tok) and a pop-up when a
// worker fails.
//
// Everything here is observation of the feed the TUI already consumes —
// local socket writes, zero supervisor tokens (invariant 2). Outside herdr
// (HERDR_ENV unset) the package does nothing.
package herdr

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Agent is the agent label herdr shows for the strawboss pane.
const Agent = "strawboss"

// source identifies strawboss's reports; herdr keys report authority on it.
const source = "strawboss"

// StripEnv returns env without herdr's pane context (HERDR_*). Children
// strawboss spawns — the supervisor's claude, managed opencode servers,
// dsh workers — would otherwise inherit HERDR_PANE_ID, and herdr's own
// claude/opencode integrations inside them would claim the strawboss pane
// as theirs: the headless supervisor session registered as the pane's
// claude (and resumed into it on a herdr restart), worker sessions
// flipping the pane between working and idle.
func StripEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "HERDR_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Client talks to the herdr server socket on behalf of one pane.
type Client struct {
	Socket string
	Pane   string
	// Timeout bounds each request (dial, write and reply). Default 500ms —
	// herdr's own integrations use the same budget.
	Timeout time.Duration

	seq atomic.Uint64
}

// FromEnv returns a client for the calling pane, or nil when strawboss is
// not running inside herdr.
func FromEnv() *Client {
	if os.Getenv("HERDR_ENV") != "1" {
		return nil
	}
	sock, pane := os.Getenv("HERDR_SOCKET_PATH"), os.Getenv("HERDR_PANE_ID")
	if sock == "" || pane == "" {
		return nil
	}
	return &Client{Socket: sock, Pane: pane}
}

// call sends one request and waits for its reply line. herdr orders
// reports by seq, so it is taken from the clock (like herdr's scripts) and
// kept strictly increasing: a restarted strawboss must outrank the
// previous run's last report.
func (c *Client) call(method string, params map[string]any) error {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	now := uint64(time.Now().UnixNano())
	seq := c.seq.Add(1)
	if seq < now {
		c.seq.CompareAndSwap(seq, now)
		seq = now
	}
	params["pane_id"] = c.Pane
	params["source"] = source
	params["seq"] = seq
	req, err := json.Marshal(map[string]any{
		"id":     fmt.Sprintf("%s:%d", source, seq),
		"method": method,
		"params": params,
	})
	if err != nil {
		return fmt.Errorf("herdr %s: encoding: %w", method, err)
	}
	conn, err := net.DialTimeout("unix", c.Socket, timeout)
	if err != nil {
		return fmt.Errorf("herdr %s: %w", method, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return fmt.Errorf("herdr %s: writing: %w", method, err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return fmt.Errorf("herdr %s: reading reply: %w", method, err)
	}
	var resp struct {
		Error *struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("herdr %s: decoding reply: %w", method, err)
	}
	if resp.Error != nil {
		return fmt.Errorf("herdr %s: %s: %s", method, resp.Error.Code, resp.Error.Message)
	}
	return nil
}

// ReportState sets the pane's agent state: "idle", "working", "blocked"
// or "unknown". message is optional context ("" omits it).
func (c *Client) ReportState(state, message string) error {
	p := map[string]any{"agent": Agent, "state": state}
	if message != "" {
		p["message"] = message
	}
	return c.call("pane.report_agent", p)
}

// ReportMetadata sets the pane title and sidebar tokens. A nil token
// value clears that token; an empty title clears the title.
func (c *Client) ReportMetadata(title string, tokens map[string]*string) error {
	p := map[string]any{"agent": Agent, "tokens": tokens}
	if title != "" {
		p["title"] = title
	} else {
		p["clear_title"] = true
	}
	return c.call("pane.report_metadata", p)
}

// Notify shows a herdr pop-up.
func (c *Client) Notify(title, body string) error {
	p := map[string]any{"title": title, "sound": "request"}
	if body != "" {
		p["body"] = body
	}
	// notification.show is not pane-scoped; call adds pane/source/seq,
	// which herdr ignores for it.
	return c.call("notification.show", p)
}

// Release hands the pane back: the agent label goes away. Metadata
// outlives a release (verified against herdr 0.9.1), so callers clear
// tokens first.
func (c *Client) Release() error {
	return c.call("pane.release_agent", map[string]any{"agent": Agent})
}
