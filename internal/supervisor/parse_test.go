package supervisor

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseFixture parses every line of a captured stream file, skipping
// nil (uninteresting) events.
func parseFixture(t *testing.T, name string) []Event {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxLineBytes)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		ev, err := ParseLine(sc.Bytes())
		if err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
		if ev != nil {
			events = append(events, ev)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

const wantSession = "35a53d5b-ee5c-4624-9f59-afb4d9e34f26"

// turn1.jsonl: real captured stream of a first turn — plain text reply,
// no tools. rate_limit, init, assistant, rate_limit, result.
func TestParseTurn1(t *testing.T) {
	events := parseFixture(t, "turn1.jsonl")
	if len(events) != 5 {
		t.Fatalf("got %d events, want 5: %#v", len(events), events)
	}

	rl, ok := events[0].(RateLimitEvent)
	if !ok {
		t.Fatalf("events[0] = %T, want RateLimitEvent", events[0])
	}
	if rl.Status != "allowed" {
		t.Errorf("rate limit status = %q", rl.Status)
	}
	if rl.FiveHour.Utilization != 0.04 || rl.SevenDay.Utilization != 0.11 {
		t.Errorf("utilization = %v / %v, want 0.04 / 0.11", rl.FiveHour.Utilization, rl.SevenDay.Utilization)
	}
	if rl.FiveHour.ResetsAt.Unix() != 1787961600 {
		t.Errorf("five-hour resetsAt = %v", rl.FiveHour.ResetsAt.Unix())
	}

	init, ok := events[1].(InitEvent)
	if !ok {
		t.Fatalf("events[1] = %T, want InitEvent", events[1])
	}
	if init.SessionID != wantSession {
		t.Errorf("session = %q", init.SessionID)
	}
	if init.APIKeySource != "none" {
		t.Errorf("apiKeySource = %q, want none (subscription auth)", init.APIKeySource)
	}
	if init.Model != "claude-fable-5" {
		t.Errorf("model = %q", init.Model)
	}

	as, ok := events[2].(AssistantEvent)
	if !ok {
		t.Fatalf("events[2] = %T, want AssistantEvent", events[2])
	}
	if as.Text != "strawboss spike ok" {
		t.Errorf("text = %q", as.Text)
	}
	if len(as.ToolUses) != 0 {
		t.Errorf("tool uses = %v, want none", as.ToolUses)
	}
	if as.Usage.CacheReadTokens != 10125 || as.Usage.CacheCreationTokens != 7675 {
		t.Errorf("usage = %+v", as.Usage)
	}

	res, ok := events[4].(ResultEvent)
	if !ok {
		t.Fatalf("events[4] = %T, want ResultEvent", events[4])
	}
	if res.Subtype != "success" || res.IsError {
		t.Errorf("result = %+v", res)
	}
	if res.Result != "strawboss spike ok" {
		t.Errorf("result text = %q", res.Result)
	}
	if res.TotalCostUSD < 0.16 || res.TotalCostUSD > 0.17 {
		t.Errorf("cost = %v", res.TotalCostUSD)
	}
	if res.NumTurns != 1 {
		t.Errorf("num turns = %d", res.NumTurns)
	}
	if res.Usage.OutputTokens != 10 {
		t.Errorf("output tokens = %d", res.Usage.OutputTokens)
	}
}

// turn2.jsonl: real captured --resume turn with --include-partial-messages —
// a Bash tool_use, its tool_result, streamed text deltas, final text.
func TestParseTurn2(t *testing.T) {
	events := parseFixture(t, "turn2.jsonl")

	var (
		inits    []InitEvent
		statuses []StatusEvent
		asst     []AssistantEvent
		results  []ToolResultsEvent
		deltas   []StreamDeltaEvent
		finals   []ResultEvent
		unknowns []UnknownEvent
	)
	for _, ev := range events {
		switch e := ev.(type) {
		case InitEvent:
			inits = append(inits, e)
		case StatusEvent:
			statuses = append(statuses, e)
		case AssistantEvent:
			asst = append(asst, e)
		case ToolResultsEvent:
			results = append(results, e)
		case StreamDeltaEvent:
			deltas = append(deltas, e)
		case ResultEvent:
			finals = append(finals, e)
		case UnknownEvent:
			unknowns = append(unknowns, e)
		}
	}

	if len(unknowns) != 0 {
		t.Errorf("unknown events: %v", unknowns)
	}
	if len(inits) != 1 || inits[0].SessionID != wantSession {
		t.Errorf("inits = %+v", inits)
	}
	if len(statuses) != 2 {
		t.Errorf("got %d status events, want 2", len(statuses))
	}

	// Delegation detection: the Bash tool_use surfaces name + command.
	if len(asst) != 2 {
		t.Fatalf("got %d assistant events, want 2", len(asst))
	}
	if len(asst[0].ToolUses) != 1 {
		t.Fatalf("first assistant tool uses = %+v", asst[0].ToolUses)
	}
	tu := asst[0].ToolUses[0]
	if tu.Name != "Bash" {
		t.Errorf("tool name = %q", tu.Name)
	}
	cmd, ok := tu.BashCommand()
	if !ok || cmd != "echo delegation-test-w1" {
		t.Errorf("BashCommand = %q, %v", cmd, ok)
	}

	// The matching tool_result closes the loop, keyed by tool_use id.
	if len(results) != 1 || len(results[0].Results) != 1 {
		t.Fatalf("tool results = %+v", results)
	}
	tr := results[0].Results[0]
	if tr.ToolUseID != tu.ID {
		t.Errorf("tool_use_id %q != tool use id %q", tr.ToolUseID, tu.ID)
	}
	if tr.Content != "delegation-test-w1" || tr.IsError {
		t.Errorf("tool result = %+v", tr)
	}

	// Partial text streams in as deltas and matches the final text.
	if len(deltas) == 0 {
		t.Fatal("no stream deltas parsed")
	}
	streamed := ""
	for _, d := range deltas {
		streamed += d.Text
	}
	if streamed != asst[1].Text {
		t.Errorf("streamed text %q != final text %q", streamed, asst[1].Text)
	}
	if !strings.Contains(asst[1].Text, "strawboss spike ok") {
		t.Errorf("resume lost context: final text = %q", asst[1].Text)
	}

	if len(finals) != 1 || finals[0].IsError {
		t.Fatalf("finals = %+v", finals)
	}
	if finals[0].NumTurns != 3 && finals[0].NumTurns < 1 {
		t.Errorf("num turns = %d", finals[0].NumTurns)
	}
}

func TestParseLineEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		line string
		want func(t *testing.T, ev Event)
	}{
		{
			name: "garbage is unknown, not an error",
			line: "not json at all",
			want: func(t *testing.T, ev Event) {
				u, ok := ev.(UnknownEvent)
				if !ok || u.Err == nil {
					t.Errorf("got %#v", ev)
				}
			},
		},
		{
			name: "unrecognized type preserved",
			line: `{"type":"shiny_new_thing","x":1}`,
			want: func(t *testing.T, ev Event) {
				u, ok := ev.(UnknownEvent)
				if !ok || u.Type != "shiny_new_thing" {
					t.Errorf("got %#v", ev)
				}
			},
		},
		{
			name: "tool_result with block-list content",
			line: `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"is_error":true}]}}`,
			want: func(t *testing.T, ev Event) {
				tr, ok := ev.(ToolResultsEvent)
				if !ok || len(tr.Results) != 1 {
					t.Fatalf("got %#v", ev)
				}
				if tr.Results[0].Content != "ab" || !tr.Results[0].IsError {
					t.Errorf("got %+v", tr.Results[0])
				}
			},
		},
		{
			name: "user line without tool results is skipped",
			line: `{"type":"user","message":{"role":"user","content":"hi"}}`,
			want: func(t *testing.T, ev Event) {
				if ev != nil {
					t.Errorf("got %#v, want nil", ev)
				}
			},
		},
		{
			name: "non-text stream delta is skipped",
			line: `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}}`,
			want: func(t *testing.T, ev Event) {
				if ev != nil {
					t.Errorf("got %#v, want nil", ev)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, err := ParseLine([]byte(tt.line))
			if err != nil {
				t.Fatal(err)
			}
			tt.want(t, ev)
		})
	}
}

func TestUsageAddTotal(t *testing.T) {
	a := Usage{InputTokens: 1, OutputTokens: 2, CacheCreationTokens: 3, CacheReadTokens: 4}
	b := a.Add(a)
	if b != (Usage{2, 4, 6, 8}) {
		t.Errorf("Add = %+v", b)
	}
	if a.Total() != 10 {
		t.Errorf("Total = %d", a.Total())
	}
}
