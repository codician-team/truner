package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpsertINISection(t *testing.T) {
	p := filepath.Join(t.TempDir(), "credentials")
	orig := "[default]\naws_access_key_id = AAA\naws_secret_access_key = BBB\n\n[truner]\naws_access_key_id = OLD\n\n[other]\nregion = eu-west-1\n"
	os.WriteFile(p, []byte(orig), 0o600)
	if err := upsertINISection(p, "truner", [][2]string{{"aws_access_key_id", "NEW"}, {"aws_secret_access_key", "S"}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	s := string(data)
	for _, want := range []string{"[default]\naws_access_key_id = AAA", "[other]\nregion = eu-west-1", "[truner]\naws_access_key_id = NEW\naws_secret_access_key = S"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "OLD") || strings.Count(s, "[truner]") != 1 {
		t.Errorf("old section not replaced:\n%s", s)
	}
	// Creating a new file works too.
	q := filepath.Join(t.TempDir(), "sub", "config")
	if err := upsertINISection(q, "profile truner", [][2]string{{"region", "us-east-1"}}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(q); string(data) != "[profile truner]\nregion = us-east-1\n" {
		t.Errorf("new file: %q", data)
	}
}

func TestAppName(t *testing.T) {
	if n := appName("Acme", "truner"); n != "truner-acme" {
		t.Errorf("got %q", n)
	}
	if n := appName("a-very-long-organization-name-indeed", "prod"); len(n) > 34 {
		t.Errorf("app names are limited to 34 characters: %q", n)
	}
	if n := appName("", "truner"); !strings.HasPrefix(n, "truner-") || len(n) != 13 {
		t.Errorf("personal accounts get a random suffix: %q", n)
	}
}
