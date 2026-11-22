package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"j-cicd/internal/config"
)

func digest(repo, c string) string { return repo + "@sha256:" + strings.Repeat(c, 64) }

// What the build host sends (per-commit tags), and the digests the fake
// registry resolves them to: tag "cN" is digest N.
var (
	beV1, beV1Digest = "ghcr.io/o/be:c1", digest("ghcr.io/o/be", "1")
	beV2, beV2Digest = "ghcr.io/o/be:c2", digest("ghcr.io/o/be", "2")
	feV1, feV1Digest = "ghcr.io/o/fe:c1", digest("ghcr.io/o/fe", "1")
)

// fakeDocker stands in for docker: it resolves pulled tags to digests,
// remembers which ref each image variable was last `up`ed with, and
// reports container health per ref from health (default "running
// healthy").
type fakeDocker struct {
	calls   []string
	running map[string]string // image var -> ref
	health  map[string]string // ref -> "status health"
	failOn  string
	hangOn  string // a matching command blocks until cancelled
}

func (f *fakeDocker) Run(ctx context.Context, dir, name string, args ...string) error {
	cmd := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, cmd)
	if f.failOn != "" && strings.Contains(cmd, f.failOn) {
		return errors.New("exit status 1")
	}
	if f.hangOn != "" && strings.Contains(cmd, f.hangOn) {
		<-ctx.Done()
		return errors.New("signal: killed")
	}
	if strings.Contains(cmd, " up -d ") {
		if f.running == nil {
			f.running = map[string]string{}
		}
		for _, a := range args {
			if k, v, ok := strings.Cut(a, "="); ok && strings.HasSuffix(k, "_IMAGE") {
				f.running[k] = v
			}
		}
	}
	return nil
}

var serviceVars = map[string]string{"backend": "BACKEND_IMAGE", "frontend": "FRONTEND_IMAGE"}

func (f *fakeDocker) Output(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	if name == "env" { // compose ps -a -q <svc>
		return []byte("cid-" + args[len(args)-1] + "\n"), nil
	}
	if args[0] == "image" { // docker image inspect --format ... repo:cN
		ref := args[len(args)-1]
		i := strings.LastIndex(ref, ":")
		repo, tag := ref[:i], ref[i+1:]
		return []byte(digest("other.example/x", "0") + "\n" + digest(repo, tag[len(tag)-1:]) + "\n"), nil
	}
	// docker inspect --format ... cid-<svc>
	svc := strings.TrimPrefix(args[len(args)-1], "cid-")
	ref := f.running[serviceVars[svc]]
	if h, ok := f.health[ref]; ok {
		return []byte(h + "\n"), nil
	}
	return []byte("running healthy\n"), nil
}

func (f *fakeDocker) composeCalls(verb string) []string {
	var out []string
	for _, c := range f.calls {
		if strings.Contains(c, " compose ") && strings.Contains(c, " "+verb+" ") {
			out = append(out, c)
		}
	}
	return out
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{
		StateDir: t.TempDir(),
		Projects: []Project{{
			Name:          "st",
			ComposeFile:   "/opt/st/compose.yml",
			EnvFile:       "/opt/st/.env",
			HealthTimeout: &config.Duration{Duration: 200 * time.Millisecond},
			Services: []Service{
				{Name: "backend", ImageVar: "BACKEND_IMAGE", Repository: "ghcr.io/o/be"},
				{Name: "frontend", ImageVar: "FRONTEND_IMAGE", Repository: "ghcr.io/o/fe"},
			},
		}},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return cfg
}

func testAgent(cfg *Config, docker *fakeDocker) *Agent {
	a := New(cfg, Options{Runner: docker, Logger: log.New(io.Discard, "", 0)})
	a.pollInterval = time.Millisecond
	return a
}

func state(t *testing.T, cfg *Config) *State {
	t.Helper()
	st, err := LoadState(cfg.StateDir, "st")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return st
}

func TestFirstDeployPullsStartsAndRecords(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{}
	if err := testAgent(cfg, docker).Deploy(context.Background(), "st", []string{beV1, feV1}, nil); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// Pulled by tag, then run and recorded by the digest it resolved to.
	compose := " docker compose -f /opt/st/compose.yml --env-file /opt/st/.env "
	want := []string{
		"env BACKEND_IMAGE=" + beV1 + " FRONTEND_IMAGE=" + feV1 + compose + "pull backend frontend",
		"env BACKEND_IMAGE=" + beV1Digest + " FRONTEND_IMAGE=" + feV1Digest + compose + "up -d --no-deps backend frontend",
	}
	got := append(docker.composeCalls("pull"), docker.composeCalls("up")...)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("commands:\n got %q\nwant %q", got, want)
	}

	be := state(t, cfg).Services["backend"]
	if be.Current != beV1Digest || be.CurrentTag != "c1" || be.Previous != "" || be.LastResult != resultHealthy {
		t.Errorf("backend state = %+v", be)
	}
}

func TestDeployPinsUntouchedServices(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{}
	a := testAgent(cfg, docker)
	if err := a.Deploy(context.Background(), "st", []string{beV1, feV1}, nil); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	docker.calls = nil
	if err := a.Deploy(context.Background(), "st", []string{beV2}, nil); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}

	// Only backend is recreated, but frontend's variable is still pinned
	// to its recorded release rather than left to the compose default.
	up := docker.composeCalls("up")
	if len(up) != 1 || !strings.Contains(up[0], "FRONTEND_IMAGE="+feV1Digest) || !strings.HasSuffix(up[0], "--no-deps backend") {
		t.Errorf("up = %q", up)
	}
	be := state(t, cfg).Services["backend"]
	if be.Current != beV2Digest || be.CurrentTag != "c2" || be.Previous != beV1Digest || be.PreviousTag != "c1" {
		t.Errorf("backend state = %+v, want current c2, previous c1", be)
	}
}

func TestDeploySameReleaseIsANoop(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{}
	a := testAgent(cfg, docker)
	if err := a.Deploy(context.Background(), "st", []string{beV1}, nil); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	docker.calls = nil
	if err := a.Deploy(context.Background(), "st", []string{beV1}, nil); err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	if up := docker.composeCalls("up"); len(up) != 0 {
		t.Errorf("redeploying the running release recreated containers: %q", up)
	}
}

func TestUnhealthyReleaseRollsBack(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{health: map[string]string{beV2Digest: "running unhealthy"}}
	a := testAgent(cfg, docker)
	if err := a.Deploy(context.Background(), "st", []string{beV1}, nil); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}

	err := a.Deploy(context.Background(), "st", []string{beV2}, nil)
	if err == nil || !strings.Contains(err.Error(), "unhealthy") || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v, want an unhealthy deploy that was rolled back", err)
	}
	if got := docker.running["BACKEND_IMAGE"]; got != beV1Digest {
		t.Errorf("backend running %s after rollback, want %s", got, beV1Digest)
	}
	be := state(t, cfg).Services["backend"]
	if be.Current != beV1Digest || be.CurrentTag != "c1" || be.LastAttempt != beV2 || be.LastResult != resultRolledBack {
		t.Errorf("backend state = %+v", be)
	}
}

func TestFailedRollbackIsLoud(t *testing.T) {
	cfg := testConfig(t)
	docker := &fakeDocker{health: map[string]string{beV2Digest: "running unhealthy"}}
	a := testAgent(cfg, docker)
	if err := a.Deploy(context.Background(), "st", []string{beV1}, nil); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	docker.health[beV1Digest] = "exited none" // the old release no longer comes up either

	err := a.Deploy(context.Background(), "st", []string{beV2}, nil)
	if err == nil || !strings.Contains(err.Error(), "ROLLBACK FAILED") {
		t.Fatalf("err = %v, want a failed rollback", err)
	}
	if got := state(t, cfg).Services["backend"].LastResult; got != resultRollbackFailed {
		t.Errorf("last result = %q", got)
	}
}

func TestFailedFirstDeployHasNothingToRollBackTo(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{health: map[string]string{beV1Digest: "restarting none"}}
	err := testAgent(cfg, docker).Deploy(context.Background(), "st", []string{beV1}, nil)
	if err == nil || !strings.Contains(err.Error(), "no previous release") {
		t.Fatalf("err = %v, want no rollback target", err)
	}
	be := state(t, cfg).Services["backend"]
	if be.Current != "" || be.LastResult != resultNoRollback {
		t.Errorf("backend state = %+v", be)
	}
}

func TestHealthTimeout(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{health: map[string]string{beV2Digest: "running starting"}}
	a := testAgent(cfg, docker)
	if err := a.Deploy(context.Background(), "st", []string{beV1}, nil); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	err := a.Deploy(context.Background(), "st", []string{beV2}, nil)
	if err == nil || !strings.Contains(err.Error(), "not healthy after 200ms") {
		t.Fatalf("err = %v, want a health timeout", err)
	}
	if got := docker.running["BACKEND_IMAGE"]; got != beV1Digest {
		t.Errorf("backend running %s after timeout, want rollback to %s", got, beV1Digest)
	}
}

func TestFailedPullChangesNothing(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{}
	a := testAgent(cfg, docker)
	if err := a.Deploy(context.Background(), "st", []string{beV1}, nil); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	docker.calls, docker.failOn = nil, " pull "

	err := a.Deploy(context.Background(), "st", []string{beV2}, nil)
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v, want a pull failure that changed nothing", err)
	}
	if up := docker.composeCalls("up"); len(up) != 0 {
		t.Errorf("started containers after a failed pull: %q", up)
	}
	if be := state(t, cfg).Services["backend"]; be.Current != beV1Digest {
		t.Errorf("current = %s, want unchanged %s", be.Current, beV1Digest)
	}
}

func TestDeployRejectsBadRefs(t *testing.T) {
	cases := map[string]struct {
		project string
		refs    []string
		want    string
	}{
		"unknown project": {"nope", []string{beV1}, `no project "nope"`},
		// The silent image-name mismatch from DEPLOY.md, made loud.
		"untracked repository": {"st", []string{"ghcr.io/o/st-backend:c1"}, "no service for repository ghcr.io/o/st-backend"},
		"latest":               {"st", []string{"ghcr.io/o/be:latest"}, "not latest"},
		"no tag or digest":     {"st", []string{"ghcr.io/o/be"}, "neither repository:tag"},
		"registry port only":   {"st", []string{"registry:5000/o/be"}, "neither repository:tag"},
		"same service twice":   {"st", []string{beV1, beV2}, "given two images"},
		"nothing":              {"st", nil, "no images"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			docker := &fakeDocker{}
			err := testAgent(testConfig(t), docker).Deploy(context.Background(), tc.project, tc.refs, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
			if len(docker.calls) != 0 {
				t.Errorf("ran %q for a rejected deploy", docker.calls)
			}
		})
	}
}

func TestLockWaitsForConcurrentDeploy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "st.lock")
	unlock, err := lockFile(context.Background(), path, func() {})
	if err != nil {
		t.Fatalf("lockFile: %v", err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	waited := false
	if _, err := lockFile(ctx, path, func() { waited = true }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock err = %v, want it to wait until the deadline", err)
	}
	if !waited {
		t.Error("second deploy didn't report that it was waiting")
	}
}

func TestDeployByDigestSkipsResolution(t *testing.T) {
	// A digest can be deployed directly (e.g. a manual rollback with
	// j-cicd-agent deploy), with no tag lookup.
	cfg, docker := testConfig(t), &fakeDocker{}
	if err := testAgent(cfg, docker).Deploy(context.Background(), "st", []string{beV1Digest}, nil); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	for _, c := range docker.calls {
		if strings.HasPrefix(c, "docker image inspect") {
			t.Errorf("resolved a digest that was given: %q", c)
		}
	}
	if be := state(t, cfg).Services["backend"]; be.Current != beV1Digest || be.CurrentTag != "" {
		t.Errorf("backend state = %+v", be)
	}
}

func TestParseRefWithRegistryPort(t *testing.T) {
	repo, tag, _, err := parseRef("registry:5000/o/be:abc123")
	if err != nil || repo != "registry:5000/o/be" || tag != "abc123" {
		t.Errorf("parseRef = %q, %q, %v", repo, tag, err)
	}
}

func TestShortDigest(t *testing.T) {
	if got := ShortDigest(beV1Digest); got != "sha256:111111111111" {
		t.Errorf("ShortDigest = %q", got)
	}
}
