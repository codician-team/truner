package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codician-team/truner/internal/agentcfg"
)

func TestMicroVMHooks(t *testing.T) {
	a, _ := New(&agentcfg.Config{Mode: agentcfg.ModeMicro}, "")
	m := &microVM{a: a, ctx: context.Background(), done: make(chan struct{})}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	post := func(hook, body string) int {
		t.Helper()
		resp, err := http.Post(srv.URL+MicroHookPath+hook, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if c := post("ready", ""); c != http.StatusServiceUnavailable {
		t.Fatalf("ready while preparing: %d", c)
	}
	m.ready.Store(hookOK)
	if c := post("ready", ""); c != http.StatusOK {
		t.Fatalf("ready when prepared: %d", c)
	}
	m.ready.Store(hookFailed)
	if c := post("ready", ""); c != http.StatusInternalServerError {
		t.Fatalf("ready after a failure: %d", c)
	}

	if c := post("run", `{"microvmId":"mvm-1","runHookPayload":"not a config"}`); c != http.StatusBadRequest {
		t.Fatalf("run with a bad payload: %d", c)
	}
	enc, _ := agentcfg.Encode(&agentcfg.Config{Mode: agentcfg.ModeRun, RunnerName: "truner-1"})
	cfgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cfg" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(enc))
	}))
	defer cfgSrv.Close()
	if c := post("run", `{"microvmId":"mvm-1","runHookPayload":"`+cfgSrv.URL+`/missing"}`); c != http.StatusBadRequest {
		t.Fatalf("run with an unreadable config URL: %d", c)
	}
	m.started.Store(true) // a job is already running
	for _, payload := range []string{enc, cfgSrv.URL + "/cfg"} {
		if c := post("run", `{"microvmId":"mvm-1","runHookPayload":"`+payload+`"}`); c != http.StatusConflict {
			t.Fatalf("second run (%.20s): %d", payload, c)
		}
	}
	for _, h := range []string{"suspend", "resume", "terminate"} {
		if c := post(h, ""); c != http.StatusOK {
			t.Fatalf("%s: %d", h, c)
		}
	}
	if resp, _ := http.Get(srv.URL + MicroHookPath + "ready"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("hooks are POST only: %d", resp.StatusCode)
	}
}

func TestMicroBanner(t *testing.T) {
	a, _ := New(&agentcfg.Config{Mode: agentcfg.ModeMicro, Region: "us-east-1", MicroVCPU: 2, ImageName: "micro", Job: agentcfg.JobInfo{Spec: "MicroVM 2 vCPU / 4 GiB"}}, "")
	a.meta = Metadata{InstanceID: "mvm-1"}
	b := a.banner(false)
	for _, want := range []string{"Lambda MicroVM in us-east-1 | 2 vCPU, 4 GiB, bursting to 8 vCPU, 16 GiB", "Machine:    Lambda MicroVM mvm-1 in us-east-1", "micro (MicroVM snapshot)", "resumed from a snapshot"} {
		if !strings.Contains(b, want) {
			t.Errorf("banner lacks %q:\n%s", want, b)
		}
	}
}
