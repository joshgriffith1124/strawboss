package ui

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func TestMain(m *testing.M) {
	// Colors are otherwise stripped in test runs (no TTY), and the amber
	// threshold test reads them from the rendered label.
	termenv.SetDefaultOutput(termenv.NewOutput(os.Stdout, termenv.WithProfile(termenv.TrueColor)))
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

// TestDeadlineMerge: a deadline-carrying upsert must not wipe other
// fields, and other upserts must not wipe a known deadline.
func TestDeadlineMerge(t *testing.T) {
	dl := time.Now().Add(10 * time.Minute)
	m := New(make(chan tea.Msg))
	m = apply(t, m,
		WorkerUpsertMsg{ID: "w1", Model: "qwen-coder", Task: "t1", Status: "running"},
		// Bare deadline upsert (registry "deadline" event): only ID+Deadline.
		WorkerUpsertMsg{ID: "w1", Deadline: dl},
	)
	w := m.worker("w1")
	if w == nil || w.Deadline != dl || w.Model != "qwen-coder" || w.Status != "running" || w.Task != "t1" {
		t.Fatalf("after bare deadline upsert: %+v", w)
	}
	// A later upsert without a deadline keeps the known one.
	m = apply(t, m, WorkerUpsertMsg{ID: "w1", Status: "running", Summary: "half done"})
	w = m.worker("w1")
	if w == nil || w.Deadline != dl {
		t.Fatalf("deadline wiped by later upsert: %+v", w)
	}
}

// TestCountdownLabel: minutes left rounded up; <1m under a minute; 0m
// once past; amber under five minutes, dim otherwise.
func TestCountdownLabel(t *testing.T) {
	m := New(make(chan tea.Msg))
	m.now = time.Now()
	tests := []struct {
		name  string
		left  time.Duration
		want  string
		amber bool
	}{
		{"hour", 60 * time.Minute, "⏱ 60m", false},
		{"four minutes", 4 * time.Minute, "⏱ 4m", true},
		{"five minutes minus a tick", 5*time.Minute - time.Nanosecond, "⏱ 5m", true},
		{"under a minute", 30 * time.Second, "⏱ <1m", true},
		{"just past", -time.Second, "⏱ 0m", true},
		{"long past", -5 * time.Minute, "⏱ 0m", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := m.now.Add(tt.left)
			label, style := m.countdownLabel(d)
			if label != tt.want {
				t.Errorf("label = %q, want %q", label, tt.want)
			}
			amber := strings.Contains(style.Render("x"), "210;153;34") // amber fg
			if amber != tt.amber {
				t.Errorf("amber = %v, want %v", amber, tt.amber)
			}
		})
	}
}

// TestRolesToggleKey: ctrl+r asks for the opposite of the current mode;
// the indicator follows the orchestrator's RolesModeMsg, not the key.
func TestRolesToggleKey(t *testing.T) {
	m := New(make(chan tea.Msg))
	m = apply(t, m, tea.WindowSizeMsg{Width: 132, Height: 40})
	var asked []bool
	m.OnToggleRoles = func(on bool) { asked = append(asked, on) }

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("ctrl+r produced no cmd")
	}
	m = apply(t, m, cmd())
	if len(asked) != 1 || !asked[0] {
		t.Fatalf("hook got %v, want [true]", asked)
	}
	if m.rolesOn {
		t.Error("indicator turned on before the orchestrator confirmed")
	}
	m = apply(t, m, RolesModeMsg{On: true})
	if !m.rolesOn || !strings.Contains(m.View(), "roles on") {
		t.Error("RolesModeMsg{On: true} did not show the indicator")
	}

	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = next.(Model)
	m = apply(t, m, cmd())
	if len(asked) != 2 || asked[1] {
		t.Fatalf("second press asked %v, want off", asked)
	}
}

// TestExtendKey: '+' on a selected worker emits ExtendWorkerMsg, which
// reaches the OnExtendWorker hook; on a finished worker it refuses with
// a toast.
func TestExtendKey(t *testing.T) {
	m := New(make(chan tea.Msg))
	m = apply(t, m, tea.WindowSizeMsg{Width: 132, Height: 40},
		WorkerUpsertMsg{ID: "w1", Model: "m1", Status: "running"},
		WorkerUpsertMsg{ID: "w2", Model: "m1", Status: "failed"},
	)
	var extended string
	m.OnExtendWorker = func(id string) { extended = id }

	// Select w1 (running) and press +. The input is focused by default
	// (supervisor pane) — the worker keys are live on the dashboard tab,
	// so switch there first.
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyTab})
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("+")})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("+ on running worker produced no cmd")
	}
	em, ok := cmd().(ExtendWorkerMsg)
	if !ok || em.ID != "w1" {
		t.Fatalf("cmd msg = %#v, want ExtendWorkerMsg{w1}", cmd())
	}
	m = apply(t, m, em)
	if extended != "w1" {
		t.Errorf("hook got %q, want w1", extended)
	}

	// Move to w2 (failed): '+' must refuse with a toast.
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyDown})
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("+")})
	m = next.(Model)
	if cmd != nil {
		t.Fatalf("+ on failed worker emitted a cmd: %#v", cmd())
	}
	if !strings.Contains(m.toast, "nothing to extend") {
		t.Errorf("toast = %q", m.toast)
	}
}
