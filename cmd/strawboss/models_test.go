package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joshgriffith1124/strawboss/internal/slots"
)

func TestRunModels(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Write([]byte(`{"data":[{"id":"m1"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer up.Close()

	stateDir := t.TempDir()
	modelsPath := filepath.Join(stateDir, "models.toml")
	toml := "[models.fast]\n" +
		"endpoint = \"" + up.URL + "\"\n" +
		"model = \"m1\"\n" +
		"harness = \"dsh\"\n" +
		"tier = 1\n" +
		"max_concurrent = 2\n" +
		"\n" +
		"[models.slow]\n" +
		"endpoint = \"http://127.0.0.1:1/v1\"\n" +
		"model = \"m2\"\n" +
		"harness = \"dsh\"\n" +
		"tier = 2\n"
	if err := os.WriteFile(modelsPath, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := runModels([]string{"--state-dir", stateDir, "--models", modelsPath}, &out); err != nil {
		t.Fatalf("runModels: %v", err)
	}
	got := out.String()
	for _, want := range []string{"up", "DOWN (endpoint unreachable)", "slots 0/2 busy", "tier 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}

	release, ok, err := slots.TryAcquire(filepath.Join(stateDir, "slots"), "fast", 2)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if !ok {
		t.Fatal("expected a free slot")
	}
	defer release()

	out.Reset()
	if err := runModels([]string{"--state-dir", stateDir, "--models", modelsPath}, &out); err != nil {
		t.Fatalf("runModels (slot held): %v", err)
	}
	if !strings.Contains(out.String(), "slots 1/2 busy") {
		t.Errorf("output missing %q:\n%s", "slots 1/2 busy", out.String())
	}
}
