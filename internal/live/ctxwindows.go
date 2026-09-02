package live

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Context windows are remembered PER MODEL, across runs and projects. The
// window only ever arrives with a completed turn's result event, so a
// session that resumes — or whose first turns are interrupted — would
// otherwise sit on a guess. A model seen once is known forever after,
// from the moment system/init names it.
func modelWindowsPath(stateDir string) string {
	return filepath.Join(stateDir, "model-windows.json")
}

// loadModelWindows reads the cache; a missing or unreadable file is an
// empty map, never an error — an unknown window is displayed state.
func loadModelWindows(stateDir string) map[string]int {
	m := map[string]int{}
	if b, err := os.ReadFile(modelWindowsPath(stateDir)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// saveModelWindow records one model's window. Read-modify-write without a
// lock: two instances racing here write the same fact.
func saveModelWindow(stateDir, model string, window int) error {
	if model == "" || window <= 0 {
		return nil
	}
	m := loadModelWindows(stateDir)
	if m[model] == window {
		return nil
	}
	m[model] = window
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding model windows: %w", err)
	}
	path := modelWindowsPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("saving model windows: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("saving model windows: %w", err)
	}
	return nil
}
