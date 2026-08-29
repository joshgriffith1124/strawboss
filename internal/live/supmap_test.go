package live

import (
	"encoding/json"
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/supervisor"
	"github.com/joshgriffith1124/strawboss/internal/ui"
)

func TestParseDelegate(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want *ui.DelegateInfo
	}{
		{
			name: "plain",
			cmd:  `strawboss delegate --model qwen-coder --task "fix the tests"`,
			want: &ui.DelegateInfo{Model: "qwen-coder", Task: "fix the tests"},
		},
		{
			name: "absolute path and single quotes",
			cmd:  `/home/josh/git/strawboss/bin/strawboss delegate --task 'scaffold the app' --model qwen-small`,
			want: &ui.DelegateInfo{Model: "qwen-small", Task: "scaffold the app"},
		},
		{
			name: "equals form",
			cmd:  `strawboss delegate --model=qwen-coder --task="a b c"`,
			want: &ui.DelegateInfo{Model: "qwen-coder", Task: "a b c"},
		},
		{
			name: "not a delegate",
			cmd:  `go test ./...`,
			want: nil,
		},
		{
			name: "strawboss but different subcommand",
			cmd:  `strawboss version`,
			want: nil,
		},
		{
			name: "delegate word in an unrelated command",
			cmd:  `grep delegate docs/DELEGATION.md`,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseDelegate(tt.cmd)
			if (got == nil) != (tt.want == nil) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			if got != nil && *got != *tt.want {
				t.Errorf("got %+v, want %+v", *got, *tt.want)
			}
		})
	}
}

func TestMapSupEvent(t *testing.T) {
	tests := []struct {
		name  string
		ev    supervisor.Event
		check func(t *testing.T, msgs []tea.Msg)
	}{
		{
			name: "init subscription",
			ev:   supervisor.InitEvent{SessionID: "s1", Model: "claude-fable-5", APIKeySource: "none"},
			check: func(t *testing.T, msgs []tea.Msg) {
				init := msgs[0].(ui.SupInitMsg)
				if init.Auth != "subscription" || init.SessionID != "s1" || init.PID != 42 {
					t.Errorf("init = %+v", init)
				}
			},
		},
		{
			name: "init with leaked api key screams",
			ev:   supervisor.InitEvent{SessionID: "s1", APIKeySource: "env"},
			check: func(t *testing.T, msgs []tea.Msg) {
				init := msgs[0].(ui.SupInitMsg)
				if init.Auth == "subscription" {
					t.Errorf("auth = %q, want warning", init.Auth)
				}
			},
		},
		{
			name: "assistant with delegate tool use",
			ev: supervisor.AssistantEvent{
				Text: "Delegating now.",
				ToolUses: []supervisor.ToolUse{{
					ID: "t1", Name: "Bash",
					Input: json.RawMessage(`{"command":"strawboss delegate --model qwen-coder --task \"do it\""}`),
				}},
			},
			check: func(t *testing.T, msgs []tea.Msg) {
				if len(msgs) != 2 {
					t.Fatalf("msgs = %#v", msgs)
				}
				if td := msgs[0].(ui.SupTextDoneMsg); td.Text != "Delegating now." {
					t.Errorf("text = %+v", td)
				}
				tool := msgs[1].(ui.SupToolMsg)
				if tool.Delegate == nil || tool.Delegate.Model != "qwen-coder" || tool.Delegate.Task != "do it" {
					t.Errorf("tool = %+v delegate = %+v", tool, tool.Delegate)
				}
			},
		},
		{
			name: "result maps to usage",
			ev: supervisor.ResultEvent{
				Usage:        supervisor.Usage{InputTokens: 5, OutputTokens: 9, CacheReadTokens: 100, CacheCreationTokens: 50},
				TotalCostUSD: 0.12,
			},
			check: func(t *testing.T, msgs []tea.Msg) {
				u := msgs[0].(ui.SupUsageMsg)
				if u.Input != 5 || u.Output != 9 || u.CacheRead != 100 || u.CacheWrite != 50 || u.Turns != 1 {
					t.Errorf("usage = %+v", u)
				}
			},
		},
		{
			name: "turn done with error and stderr",
			ev:   supervisor.TurnDoneEvent{ExitErr: errors.New("exit status 1"), Stderr: "boom"},
			check: func(t *testing.T, msgs []tea.Msg) {
				d := msgs[0].(ui.SupTurnDoneMsg)
				if d.Err == "" || d.Interrupted {
					t.Errorf("done = %+v", d)
				}
			},
		},
		{
			name: "delta clears thinking status",
			ev:   supervisor.StreamDeltaEvent{Text: "hi"},
			check: func(t *testing.T, msgs []tea.Msg) {
				if msgs[0].(ui.SupStatusMsg).Status != "" {
					t.Errorf("msgs = %#v", msgs)
				}
				if msgs[1].(ui.SupTextDeltaMsg).Text != "hi" {
					t.Errorf("msgs = %#v", msgs)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, mapSupEvent(tt.ev, 42))
		})
	}
}
