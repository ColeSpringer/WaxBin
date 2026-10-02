package pathx

import (
	"path/filepath"
	"testing"
)

// TestCollisionKey: paths that differ only by case or Unicode form share a key, on every
// platform, and other paths do not.
func TestCollisionKey(t *testing.T) {
	join := func(parts ...string) string { return filepath.Join(parts...) }
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{join("lib", "Café", "a.mp3"), join("lib", "Café", "a.mp3"), true},
		{join("lib", "Tolkien", "A.mp3"), join("lib", "tolkien", "a.mp3"), true},
		{join("lib", "x", "..", "Tolkien"), join("lib", "TOLKIEN"), true},
		{join("lib", "Café"), join("lib", "Cafe"), false},
	} {
		if got := CollisionKey(c.a) == CollisionKey(c.b); got != c.same {
			t.Errorf("CollisionKey(%q) == CollisionKey(%q) is %v, want %v", c.a, c.b, got, c.same)
		}
	}
}
