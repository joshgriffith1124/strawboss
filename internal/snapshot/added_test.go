package snapshot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAddedListsOnlyNewFiles(t *testing.T) {
	dir := tRepo(t)
	before, err := Take(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zz_repro_test.go"), []byte("repro\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rWrite(t, filepath.Join(dir, "a.txt"), "x\n")
	after, err := Take(dir)
	if err != nil {
		t.Fatal(err)
	}
	added, err := Added(dir, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0] != "zz_repro_test.go" {
		t.Fatalf("Added = %v, want [zz_repro_test.go]", added)
	}
}
