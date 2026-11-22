package pipeline

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"j-cicd/internal/config"
)

func verifyConfig(t *testing.T, url, timeout string) *config.Config {
	t.Helper()
	d := &config.Deploy{Host: "vps", Verify: []config.Verify{{URL: url, ExpectBodyContains: config.RevisionPlaceholder}}}
	if timeout != "" {
		dur, err := time.ParseDuration(timeout)
		if err != nil {
			t.Fatal(err)
		}
		d.Verify[0].Timeout = &config.Duration{Duration: dur}
	}
	return deployConfig(t, d)
}

func verifyPipeline(runner *fakeRunner) *Pipeline {
	return New("/proj", Options{
		Push: true, Deploy: true, Revision: rev, Runner: runner,
		Logger: log.New(io.Discard, "", 0), VerifyInterval: time.Millisecond,
	})
}

// The site serves the previous release for a few requests (a cache
// expiring), then the new one: the run passes, after retrying.
func TestVerifyRetriesUntilTheNewReleaseIsServed(t *testing.T) {
	var hits atomic.Int32
	var sawBust, sawNoCache atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBust.Store(r.URL.Query().Get("_jcicd") != "")
		sawNoCache.Store(r.Header.Get("Cache-Control") == "no-cache")
		if hits.Add(1) < 3 {
			io.WriteString(w, `{"commit":"oldoldoldold"}`)
			return
		}
		io.WriteString(w, `{"commit":"`+rev+`"}`)
	}))
	defer srv.Close()

	runner := &fakeRunner{}
	var progress strings.Builder
	p := verifyPipeline(runner)
	p.progress = &progress
	if err := p.Run(context.Background(), verifyConfig(t, srv.URL+"/version.json", "10s")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v := p.Verified(); len(v) != 1 || v[0].Attempts != 3 || v[0].Duration <= 0 {
		t.Errorf("Verified = %+v, want one check that took 3 attempts", v)
	}
	if !strings.Contains(progress.String(), "• verified "+srv.URL+"/version.json (3 attempts, ") {
		t.Errorf("progress = %q", progress.String())
	}
	if hits.Load() != 3 {
		t.Errorf("requests = %d, want 3", hits.Load())
	}
	if !sawBust.Load() || !sawNoCache.Load() {
		t.Errorf("cache busting query = %v, no-cache header = %v", sawBust.Load(), sawNoCache.Load())
	}
	if got := sshCalls(runner.calls); len(got) != 1 {
		t.Errorf("ssh calls = %q", got)
	}
}

func TestVerifyFailureFailsTheRunAndPointsAtRollback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"commit":"stalestalest"}`)
	}))
	defer srv.Close()

	p := verifyPipeline(&fakeRunner{})
	err := p.Run(context.Background(), verifyConfig(t, srv.URL, "20ms"))
	if err == nil {
		t.Fatal("Run succeeded against a site serving another release")
	}
	for _, want := range []string{"deployed to vps", "verify " + srv.URL, rev, "stalestalest", "j-cicd rollback demo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	// The deploy itself did happen: the record should say so.
	if !p.Deployed() {
		t.Error("Deployed = false, but the deploy stage completed")
	}
}

func TestVerifyWrongStatusAndUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	err := verifyPipeline(&fakeRunner{}).Run(context.Background(), verifyConfig(t, srv.URL, "0s"))
	if err == nil || !strings.Contains(err.Error(), "got status 502, want 200") {
		t.Errorf("err = %v, want the status mismatch", err)
	}
	srv.Close()
	err = verifyPipeline(&fakeRunner{}).Run(context.Background(), verifyConfig(t, srv.URL, "0s"))
	if err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Errorf("err = %v, want a connection failure", err)
	}
}

func TestVerifyExpandsRevisionInURL(t *testing.T) {
	var path atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { path.Store(r.URL.Path) }))
	defer srv.Close()
	cfg := deployConfig(t, &config.Deploy{Host: "vps", Verify: []config.Verify{{URL: srv.URL + "/releases/{{revision}}"}}})
	if err := verifyPipeline(&fakeRunner{}).Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if path.Load() != "/releases/"+rev {
		t.Errorf("path = %v", path.Load())
	}
}

func TestVerifyOnlyAfterADeploy(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	cfg := verifyConfig(t, srv.URL, "")

	// --no-deploy: nothing new is live, so there is nothing to verify.
	p := New("/proj", Options{Push: true, Deploy: false, Revision: rev, Runner: &fakeRunner{}, Logger: log.New(io.Discard, "", 0)})
	if err := p.Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// A failed deploy never reaches verify either.
	if err := verifyPipeline(&fakeRunner{failOn: "ssh "}).Run(context.Background(), cfg); err == nil {
		t.Fatal("want the deploy to fail")
	}
	if hits.Load() != 0 {
		t.Errorf("verify made %d requests without a successful deploy", hits.Load())
	}
}

func TestVerifyStopsWhenCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	p := New("/proj", Options{Push: true, Deploy: true, Revision: rev, Runner: &fakeRunner{}, Logger: log.New(io.Discard, "", 0), VerifyInterval: time.Hour})
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, verifyConfig(t, srv.URL, "1h")) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("cancelled run succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("verify kept waiting after cancellation")
	}
}

func TestPlanListsVerifyURLs(t *testing.T) {
	cfg := verifyConfig(t, "https://x.example.com/v/{{revision}}", "")
	plan := New("/proj", Options{Push: true, Deploy: true, Revision: rev}).Plan(cfg)
	if plan.Deploy == nil || len(plan.Deploy.Verify) != 1 || plan.Deploy.Verify[0] != "https://x.example.com/v/"+rev {
		t.Errorf("deploy = %+v", plan.Deploy)
	}
}

func TestExecRunnerEnv(t *testing.T) {
	var out strings.Builder
	r := ExecRunner{Stdout: &out, Stderr: &out, Env: []string{"J_CICD_REVISION=abc"}}
	if err := r.Run(context.Background(), "", "sh", "-c", "echo rev=$J_CICD_REVISION"); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "rev=abc" {
		t.Errorf("output = %q", out.String())
	}
}
