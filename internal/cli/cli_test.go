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

// isolateAWS points the AWS SDK and truner's state at empty files.
func isolateAWS(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"credentials", "config"} {
		os.WriteFile(filepath.Join(dir, f), nil, 0o600)
	}
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, k := range []string{"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_REGION", "AWS_DEFAULT_REGION"} {
		t.Setenv(k, "")
	}
}

func TestNoCredentialsWithoutTerminal(t *testing.T) {
	isolateAWS(t)
	var out strings.Builder
	u := &ui{out: &out}
	_, _, _, err := awsInteractive(t.Context(), u, awsOptions{Region: "us-east-1"}, false)
	if err == nil || !strings.Contains(err.Error(), "ACTION REQUIRED: run `truner login`") {
		t.Fatalf("err = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("a run without a terminal must not prompt, printed %q", out.String())
	}
}

func TestStateKeepsPendingSetups(t *testing.T) {
	isolateAWS(t)
	st := loadState()
	st.Setups = map[string]pendingSetup{"us-east-1/truner": {State: "abc", Owner: "acme"}}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	st = loadState()
	st.Stack, st.Region = "truner", "us-east-1"
	saveState(st)
	if got := loadState().Setups["us-east-1/truner"]; got.State != "abc" || got.Owner != "acme" {
		t.Fatalf("pending setup lost: %+v", got)
	}
	forgetSetup("us-east-1/truner")
	if len(loadState().Setups) != 0 {
		t.Fatal("forgetSetup did not remove the setup")
	}
}
