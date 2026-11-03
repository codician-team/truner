package version

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		c    int
		ok   bool
	}{
		// A v0.1.1 CLI against a v0.1.3 installation.
		{"v0.1.1", "v0.1.3", -1, true},
		{"v0.1.3", "v0.1.1", 1, true},
		{"v0.1.3", "v0.1.3", 0, true},
		{"v0.10.0", "v0.9.9", 1, true},
		{"v1.0.0", "v0.99.0", 1, true},
		{"v0.1.4-rc1", "v0.1.3", 1, true},
		{"v0.1.3-2-gabc123", "v0.1.3", 0, false},
		{"dev", "v0.1.3", 0, false},
		{"v0.1.3", "dev-1a2b3c", 0, false},
		{"v0.1", "v0.1.3", 0, false},
	}
	for _, c := range cases {
		if got, ok := Compare(c.a, c.b); got != c.c || ok != c.ok {
			t.Errorf("Compare(%q, %q) = %d, %v, want %d, %v", c.a, c.b, got, ok, c.c, c.ok)
		}
	}
}
