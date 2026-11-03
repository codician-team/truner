package labels

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		raw  []string
		ok   bool
		want map[string]string
	}{
		{[]string{"truner"}, true, map[string]string{}},
		{[]string{"truner/cpu=4/RAM=16/run=123"}, true, map[string]string{"cpu": "4", "ram": "16", "run": "123"}},
		{[]string{"truner=99/memory=8"}, true, map[string]string{"run": "99", "ram": "8"}},
		{[]string{"self-hosted", "truner", "family=c7a+m7a", "spot"}, true, map[string]string{"family": "c7a+m7a", "spot": "true"}},
		{[]string{"ubuntu-latest"}, false, map[string]string{}},
		{[]string{"trunerx/cpu=4"}, false, map[string]string{}},
	}
	for _, c := range cases {
		got, ok := Parse("truner", c.raw)
		if ok != c.ok {
			t.Errorf("%v: ok=%v", c.raw, ok)
			continue
		}
		if ok && !reflect.DeepEqual(got.Values, c.want) {
			t.Errorf("%v: got %v want %v", c.raw, got.Values, c.want)
		}
	}
	l, _ := Parse("truner", []string{"truner/cpu=2/colour=blue"})
	if len(l.Unknown) != 1 || l.Unknown[0] != "colour" {
		t.Errorf("unknown keys: %v", l.Unknown)
	}
	if l.Raw[0] != "truner/cpu=2/colour=blue" {
		t.Error("raw labels must be preserved exactly for runner registration")
	}
}

func TestCustomPrefix(t *testing.T) {
	if _, ok := Parse("ci", []string{"ci/cpu=2"}); !ok {
		t.Fatal("custom prefix")
	}
	if _, ok := Parse("ci", []string{"truner/cpu=2"}); ok {
		t.Fatal("other installations' labels must not match")
	}
}

func TestForeignPrefix(t *testing.T) {
	cases := []struct {
		prefix      string
		raw         []string
		label, want string
	}{
		// bake3d, 2026-11-03: the org's installation has the prefix truner-codician.
		{"truner-codician", []string{"truner/cpu=8/run=41"}, "truner/cpu=8/run=41", "truner"},
		{"truner-codician", []string{"truner-codician/cpu=8/run=41"}, "", ""},
		{"truner", []string{"truner-codician/micro/run=41"}, "truner-codician/micro/run=41", "truner-codician"},
		{"truner", []string{"self-hosted", "truner-gpu", "cpu=4"}, "truner-gpu", "truner-gpu"},
		{"ci", []string{"truner=41/cpu=2"}, "truner=41/cpu=2", "truner"},
		{"ci", []string{"CI/cpu=2"}, "CI/cpu=2", "CI"},
		{"truner", []string{"truner/cpu=2"}, "", ""},
		{"truner", []string{"ubuntu-latest"}, "", ""},
		{"truner", []string{"trunerx/cpu=4"}, "", ""},
		{"ci", []string{"ci-gpu/cpu=4"}, "", ""},
	}
	for _, c := range cases {
		if label, got := ForeignPrefix(c.prefix, c.raw); label != c.label || got != c.want {
			t.Errorf("ForeignPrefix(%q, %v) = %q, %q, want %q, %q", c.prefix, c.raw, label, got, c.label, c.want)
		}
	}
}

func TestSplitList(t *testing.T) {
	if got := SplitList("c7a+m7a, r7a"); !reflect.DeepEqual(got, []string{"c7a", "m7a", "r7a"}) {
		t.Fatalf("got %v", got)
	}
}
