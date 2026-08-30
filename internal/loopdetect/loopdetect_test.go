package loopdetect

import "testing"

func TestDetector(t *testing.T) {
	d := New(3)
	for i, key := range []string{"a", "a", "b", "b"} {
		if _, hit := d.Observe(key); hit {
			t.Fatalf("triggered early at %d", i)
		}
	}
	if n, hit := d.Observe("b"); !hit || n != 3 {
		t.Fatalf("no trigger at threshold: n=%d hit=%v", n, hit)
	}
	// Stays triggered while the run continues, resets on change.
	if _, hit := d.Observe("b"); !hit {
		t.Fatal("dropped trigger mid-run")
	}
	if _, hit := d.Observe("c"); hit {
		t.Fatal("triggered after reset")
	}
}
