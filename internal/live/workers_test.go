package live

import (
	"testing"

	"github.com/joshgriffith1124/strawboss/internal/harness/opencode"
)

func tok(in, cacheRead, cacheWrite int) opencode.Tokens {
	t := opencode.Tokens{Input: in}
	t.Cache.Read = cacheRead
	t.Cache.Write = cacheWrite
	return t
}

func TestOpencodeCtx(t *testing.T) {
	assistant := func(id string, toks opencode.Tokens) opencode.Message {
		var m opencode.Message
		m.Info.ID, m.Info.Role, m.Info.Tokens = id, "assistant", toks
		return m
	}
	user := func(id string) opencode.Message {
		var m opencode.Message
		m.Info.ID, m.Info.Role = id, "user"
		return m
	}
	tests := []struct {
		name string
		msgs []opencode.Message
		want int
	}{
		{"no messages", nil, 0},
		{"only user messages", []opencode.Message{user("u1"), user("u2")}, 0},
		{"last assistant with tokens wins", []opencode.Message{
			user("u1"),
			assistant("a1", tok(1000, 200, 30)),
			user("u2"),
			assistant("a2", tok(2000, 400, 50)),
		}, 2450},
		{"streaming last assistant falls back to previous", []opencode.Message{
			assistant("a1", tok(1000, 200, 30)),
			assistant("a2", tok(0, 0, 0)),
		}, 1230},
		{"zero-token assistant only", []opencode.Message{assistant("a1", tok(0, 0, 0))}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := opencodeCtx(tt.msgs); got != tt.want {
				t.Errorf("opencodeCtx() = %d, want %d", got, tt.want)
			}
		})
	}
}
