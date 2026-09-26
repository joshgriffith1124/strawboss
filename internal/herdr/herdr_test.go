package herdr

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/ui"
)

func TestStripEnv(t *testing.T) {
	in := []string{"PATH=/bin", "HERDR_ENV=1", "HERDR_PANE_ID=w1:p1", "HOME=/h", "HERDR_SOCKET_PATH=/s", "NOT_HERDR_X=1"}
	want := []string{"PATH=/bin", "HOME=/h", "NOT_HERDR_X=1"}
	if got := StripEnv(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("StripEnv = %v, want %v", got, want)
	}
}

func TestTracker(t *testing.T) {
	tests := []struct {
		name       string
		msgs       []tea.Msg
		wantState  string
		wantMsg    string
		wantTitle  string
		wantTokens map[string]string
		wantAlerts int
	}{
		{
			name:       "fresh is idle",
			wantState:  "idle",
			wantTokens: map[string]string{},
		},
		{
			name:       "history replay does not start a turn",
			msgs:       []tea.Msg{ui.SupUserMsg{Text: "hi"}, ui.SupTextDoneMsg{Text: "hello"}, ui.SupStatusMsg{}},
			wantState:  "idle",
			wantTokens: map[string]string{},
		},
		{
			name:       "streaming starts a turn",
			msgs:       []tea.Msg{ui.SupStatusMsg{Status: "thinking…"}},
			wantState:  "working",
			wantTokens: map[string]string{},
		},
		{
			name: "turn done with error",
			msgs: []tea.Msg{ui.SupTextDeltaMsg{Text: "x"}, ui.SupTurnDoneMsg{Err: "boom"},
				ui.SupUsageMsg{Input: 1000, Output: 500, CacheWrite: 500, CacheRead: 90000}},
			wantState:  "idle",
			wantMsg:    "boom",
			wantTokens: map[string]string{"sup_tok": "2.0k sup"},
		},
		{
			name: "delegation titles the pane; running worker keeps it working",
			msgs: []tea.Msg{
				ui.SupToolMsg{ToolID: "t1", Name: "Bash", Delegate: &ui.DelegateInfo{Model: "m", Task: "fix the parser\nsecond line"}},
				ui.WorkerUpsertMsg{ID: "w1", Status: "running"},
				ui.WorkerUpsertMsg{ID: "w2", Status: "queued"},
				ui.WorkerUsageMsg{ID: "w1", Input: 1200, Output: 300, CacheRead: 50000},
				ui.SupTurnDoneMsg{},
			},
			wantState:  "working",
			wantTitle:  "fix the parser",
			wantTokens: map[string]string{"workers": "1 running · 1 queued", "local_tok": "1.5k local"},
		},
		{
			name: "live failure alerts once; replayed failure is silent",
			msgs: []tea.Msg{
				ui.WorkerUpsertMsg{ID: "w1", Status: "running"},
				ui.WorkerUpsertMsg{ID: "w1", Status: "failed", Summary: "tests red"},
				ui.WorkerUpsertMsg{ID: "w1", Status: "failed", Summary: "tests red"},
				ui.WorkerUpsertMsg{ID: "w9", Status: "failed", Replay: true},
			},
			wantState:  "idle",
			wantTokens: map[string]string{},
			wantAlerts: 1,
		},
		{
			name: "task file stands in for the task",
			msgs: []tea.Msg{
				ui.SupToolMsg{Delegate: &ui.DelegateInfo{TaskFile: "/tmp/task.md"}},
				ui.SessionSwitchedMsg{},
			},
			wantState:  "idle",
			wantTokens: map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := NewTracker()
			alerts := 0
			for _, m := range tt.msgs {
				alerts += len(tr.Observe(m))
			}
			s := tr.Status()
			if s.State != tt.wantState || s.Message != tt.wantMsg || s.Title != tt.wantTitle {
				t.Errorf("status = %q/%q/%q, want %q/%q/%q", s.State, s.Message, s.Title, tt.wantState, tt.wantMsg, tt.wantTitle)
			}
			if !reflect.DeepEqual(s.Tokens, tt.wantTokens) {
				t.Errorf("tokens = %v, want %v", s.Tokens, tt.wantTokens)
			}
			if alerts != tt.wantAlerts {
				t.Errorf("alerts = %d, want %d", alerts, tt.wantAlerts)
			}
		})
	}
}

// fakeHerdr is a herdr socket that records every request and answers ok.
type fakeHerdr struct {
	mu   sync.Mutex
	reqs []map[string]any
}

func startFake(t *testing.T) (*fakeHerdr, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "herdr.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeHerdr{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			var req map[string]any
			_ = json.Unmarshal(line, &req)
			f.mu.Lock()
			f.reqs = append(f.reqs, req)
			f.mu.Unlock()
			_, _ = conn.Write([]byte(`{"id":"x","result":{"type":"ok"}}` + "\n"))
			conn.Close()
		}
	}()
	return f, sock
}

func (f *fakeHerdr) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.reqs {
		out = append(out, r["method"].(string))
	}
	return out
}

func (f *fakeHerdr) last(method string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.reqs) - 1; i >= 0; i-- {
		if f.reqs[i]["method"] == method {
			return f.reqs[i]["params"].(map[string]any)
		}
	}
	return nil
}

func TestClientError(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "herdr.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		_, _ = conn.Write([]byte(`{"id":"x","error":{"code":"pane_not_found","message":"no pane"}}` + "\n"))
		conn.Close()
	}()
	c := &Client{Socket: sock, Pane: "w1:p1"}
	if err := c.ReportState("idle", ""); err == nil {
		t.Fatal("want error from herdr error reply")
	}
	missing := &Client{Socket: filepath.Join(t.TempDir(), "gone.sock"), Pane: "w1:p1"}
	if err := missing.ReportState("idle", ""); err == nil {
		t.Fatal("want error for a missing socket")
	}
}

func TestTee(t *testing.T) {
	f, sock := startFake(t)
	c := &Client{Socket: sock, Pane: "w1:p7"}
	in := make(chan tea.Msg)
	out, stop := Tee(c, in)

	msgs := []tea.Msg{
		ui.SupStatusMsg{Status: "thinking…"},
		ui.WorkerUpsertMsg{ID: "w1", Status: "running"},
		ui.WorkerUpsertMsg{ID: "w1", Status: "failed", Summary: "boom"},
		ui.SupTurnDoneMsg{},
	}
	go func() {
		for _, m := range msgs {
			in <- m
		}
		close(in)
	}()
	var got []tea.Msg
	for m := range out {
		got = append(got, m)
	}
	if !reflect.DeepEqual(got, msgs) {
		t.Fatalf("tee changed the feed: %v", got)
	}

	// The writer coalesces to the latest status; wait for it to settle.
	deadline := time.Now().Add(2 * time.Second)
	for {
		p := f.last("pane.report_agent")
		n := f.last("notification.show")
		if p != nil && p["state"] == "idle" && n != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never settled; requests: %v", f.methods())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p := f.last("pane.report_agent"); p["pane_id"] != "w1:p7" || p["agent"] != "strawboss" || p["source"] != "strawboss" {
		t.Errorf("report_agent params = %v", p)
	}
	if n := f.last("notification.show"); n["title"] != "strawboss: w1 failed" {
		t.Errorf("notification = %v", n)
	}

	stop()
	stop() // idempotent
	ms := f.methods()
	if ms[len(ms)-1] != "pane.release_agent" {
		t.Errorf("last request = %s, want pane.release_agent", ms[len(ms)-1])
	}
	md := f.last("pane.report_metadata")
	tokens := md["tokens"].(map[string]any)
	for _, name := range tokenNames {
		if v, ok := tokens[name]; !ok || v != nil {
			t.Errorf("release left token %s = %v, want explicit null", name, v)
		}
	}
	if md["clear_title"] != true {
		t.Errorf("release did not clear the title: %v", md)
	}
}
