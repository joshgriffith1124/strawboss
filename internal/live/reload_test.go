package live

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/supervisor"
	"github.com/joshgriffith1124/strawboss/internal/ui"
)

const (
	modelEntryA = "[models.a]\nendpoint = \"http://127.0.0.1:1/v1\"\nmodel = \"m\"\nharness = \"dsh\"\n"
	modelEntryB = "[models.b]\nendpoint = \"http://127.0.0.1:2/v1\"\nmodel = \"n\"\nharness = \"dsh\"\n"
)

func TestModelDiff(t *testing.T) {
	byName := func(names ...string) []config.ModelConfig {
		out := make([]config.ModelConfig, 0, len(names))
		for _, n := range names {
			out = append(out, config.ModelConfig{Name: n})
		}
		return out
	}

	assertNames := func(t *testing.T, what string, got, want []string) {
		t.Helper()
		if want == nil {
			if len(got) != 0 {
				t.Errorf("%s = %v, want nil/empty", what, got)
			}
			return
		}
		if len(got) != len(want) {
			t.Errorf("%s = %v, want %v", what, got, want)
			return
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s = %v, want %v", what, got, want)
				return
			}
		}
	}

	cases := []struct {
		name        string
		prev, next  []string
		wantAdded   []string
		wantRemoved []string
	}{
		{name: "add one", prev: []string{"a"}, next: []string{"a", "b"}, wantAdded: []string{"b"}},
		{name: "remove one", prev: []string{"a", "b"}, next: []string{"a"}, wantRemoved: []string{"b"}},
		{name: "swap", prev: []string{"a", "b"}, next: []string{"b", "c"}, wantAdded: []string{"c"}, wantRemoved: []string{"a"}},
		{name: "identical sets", prev: []string{"a", "b"}, next: []string{"a", "b"}},
		{name: "added reported in next file order", prev: []string{"x"}, next: []string{"b", "a"}, wantAdded: []string{"b", "a"}, wantRemoved: []string{"x"}},
		{name: "empty prev", prev: nil, next: []string{"a"}, wantAdded: []string{"a"}},
		{name: "empty next", prev: []string{"a"}, next: nil, wantRemoved: []string{"a"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			added, removed := modelDiff(byName(tc.prev...), byName(tc.next...))
			assertNames(t, "added", added, tc.wantAdded)
			assertNames(t, "removed", removed, tc.wantRemoved)
		})
	}
}

func TestWatchModelsReloads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.toml")
	if err := os.WriteFile(path, []byte(modelEntryA), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("initial load = %d models, want 1", len(loaded))
	}

	driver := &supervisor.Driver{}
	o := New(driver, loaded, dir)
	o.ModelsPath = path
	o.PromptFor = func(ms []config.ModelConfig) string {
		return fmt.Sprintf("prompt with %d models", len(ms))
	}

	oldPoll := modelsPollEvery
	modelsPollEvery = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done // wait for the watcher to exit before touching modelsPollEvery
		modelsPollEvery = oldPoll
	})
	go func() {
		defer close(done)
		o.watchModels(ctx)
	}()

	// Give the watcher's initial stat (the baseline stamp) a moment to land
	// on the original file before we mutate it.
	time.Sleep(100 * time.Millisecond)

	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(path, []byte(modelEntryA+modelEntryB), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	// The reload log line is emitted after ModelSetMsg, so draining to it
	// guarantees both have arrived.
	msgs := drainUntil(t, o.Feed(), 15*time.Second, func(m tea.Msg) bool {
		l, ok := m.(ui.RawLogMsg)
		return ok && l.Source == "app" &&
			strings.Contains(l.Line, "models.toml reloaded") &&
			strings.Contains(l.Line, "added b")
	})

	var gotSet []string
	for _, m := range msgs {
		if set, ok := m.(ui.ModelSetMsg); ok {
			gotSet = set.Names
		}
	}
	if len(gotSet) != 2 || gotSet[0] != "a" || gotSet[1] != "b" {
		t.Errorf("ModelSetMsg Names = %#v, want [a b]", gotSet)
	}
	if got := o.Models(); len(got) != 2 {
		t.Errorf("o.Models() len = %d, want 2", len(got))
	}
	if driver.SystemPrompt != "prompt with 2 models" {
		t.Errorf("driver.SystemPrompt = %q, want %q", driver.SystemPrompt, "prompt with 2 models")
	}
}

func TestWatchModelsKeepsSetOnBadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.toml")
	if err := os.WriteFile(path, []byte(modelEntryA+modelEntryB), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("initial load = %d models, want 2", len(loaded))
	}

	driver := &supervisor.Driver{}
	o := New(driver, loaded, dir)
	o.ModelsPath = path

	oldPoll := modelsPollEvery
	modelsPollEvery = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done // wait for the watcher to exit before touching modelsPollEvery
		modelsPollEvery = oldPoll
	})
	go func() {
		defer close(done)
		o.watchModels(ctx)
	}()

	// Give the watcher's initial stat (the baseline stamp) a moment to land
	// on the original file before we mutate it.
	time.Sleep(100 * time.Millisecond)

	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(path, []byte("[models.a\nendpoint ="), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	drainUntil(t, o.Feed(), 15*time.Second, func(m tea.Msg) bool {
		to, ok := m.(ui.ToastMsg)
		return ok && strings.Contains(to.Text, "models.toml not reloaded")
	})

	if got := o.Models(); len(got) != 2 {
		t.Errorf("o.Models() len = %d after bad file, want 2", len(got))
	}
}
