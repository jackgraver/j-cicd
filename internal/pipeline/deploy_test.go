package pipeline

import (
	"context"
	"encoding/base64"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"j-cicd/internal/config"
)

const rev = "abc123456789"

// deployConfig loads a two-image config with the given deploy section,
// tagged the way a real run with revision rev would be.
func deployConfig(t *testing.T, deploy *config.Deploy) *config.Config {
	t.Helper()
	return writeAndLoad(t, &config.Config{
		Name:  "demo",
		Steps: []config.Step{{Name: "test", Run: "make test"}},
		Images: []config.Image{
			{Name: "backend", Repository: "ghcr.io/o/be"},
			{Name: "frontend", Repository: "ghcr.io/o/fe"},
		},
		Deploy: deploy,
	}, rev)
}

type runOpts struct {
	push, deploy, deployDirty bool
	revision                  string
}

func runWith(t *testing.T, runner *fakeRunner, cfg *config.Config, o runOpts) error {
	t.Helper()
	p := New("/proj", Options{
		Push: o.push, Deploy: o.deploy, Revision: o.revision, DeployDirty: o.deployDirty,
		Runner: runner, Logger: log.New(io.Discard, "", 0),
	})
	return p.Run(context.Background(), cfg)
}

var deployAll = runOpts{push: true, deploy: true, revision: rev}

func sshCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "ssh ") {
			out = append(out, c)
		}
	}
	return out
}

func TestDeployRunsLastWithPerCommitTags(t *testing.T) {
	runner := &fakeRunner{}
	cfg := deployConfig(t, &config.Deploy{Host: "deploy@vps"})
	if err := runWith(t, runner, cfg, deployAll); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := "ssh -o BatchMode=yes -- deploy@vps j-cicd-agent deploy 'demo' 'ghcr.io/o/be:" + rev + "' 'ghcr.io/o/fe:" + rev + "'"
	if last := runner.calls[len(runner.calls)-1]; last != want {
		t.Errorf("last command:\n got %q\nwant %q", last, want)
	}
	if n := len(sshCalls(runner.calls)); n != 1 {
		t.Errorf("ran ssh %d times, want once for the whole deploy", n)
	}
}

func TestDeployRestrictedToListedImages(t *testing.T) {
	runner := &fakeRunner{}
	cfg := deployConfig(t, &config.Deploy{
		Host:    "vps",
		Command: "sudo /usr/local/bin/j-cicd-agent",
		Project: "simple-track",
		Images:  []string{"frontend"},
	})
	if err := runWith(t, runner, cfg, deployAll); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"ssh -o BatchMode=yes -- vps sudo /usr/local/bin/j-cicd-agent deploy 'simple-track' 'ghcr.io/o/fe:" + rev + "'"}
	assertCalls(t, sshCalls(runner.calls), want)
}

func TestDeploySkippedWithoutPushOrWithNoDeploy(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts runOpts
	}{
		// Deploying after --no-push would roll out whatever was in the
		// registry before, while the log claims this build went out.
		{"no-push", runOpts{push: false, deploy: true, revision: rev}},
		{"no-deploy", runOpts{push: true, deploy: false, revision: rev}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			cfg := deployConfig(t, &config.Deploy{Host: "vps"})
			if err := runWith(t, runner, cfg, tc.opts); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := sshCalls(runner.calls); len(got) != 0 {
				t.Errorf("deployed anyway: %q", got)
			}
		})
	}
}

// A build that can't be deployed must fail before anything runs, not
// after a long build and a push.
func TestUndeployableBuildFailsUpFront(t *testing.T) {
	cases := map[string]struct {
		cfg  func(t *testing.T) *config.Config
		opts runOpts
		want string
	}{
		"dirty tree": {
			func(t *testing.T) *config.Config { return deployConfig(t, &config.Deploy{Host: "vps"}) },
			runOpts{push: true, deploy: true, revision: rev + "-dirty"},
			"uncommitted changes",
		},
		"no revision": {
			func(t *testing.T) *config.Config { return deployConfig(t, &config.Deploy{Host: "vps"}) },
			runOpts{push: true, deploy: true},
			"no git revision",
		},
		"explicit tags without the revision": {
			func(t *testing.T) *config.Config {
				return writeAndLoad(t, &config.Config{
					Name:   "demo",
					Steps:  []config.Step{{Run: "make"}},
					Images: []config.Image{{Name: "be", Repository: "ghcr.io/o/be", Tags: []string{"latest"}}},
					Deploy: &config.Deploy{Host: "vps"},
				}, rev)
			},
			deployAll,
			"isn't tagged",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{}
			err := runWith(t, runner, tc.cfg(t), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if len(runner.calls) != 0 {
				t.Errorf("ran %q before refusing", runner.calls)
			}
		})
	}
}

func TestDeployDirtyWhenAllowed(t *testing.T) {
	runner := &fakeRunner{}
	dirty := rev + "-dirty"
	cfg := writeAndLoad(t, &config.Config{
		Name:   "demo",
		Images: []config.Image{{Name: "be", Repository: "ghcr.io/o/be"}},
		Deploy: &config.Deploy{Host: "vps"},
	}, dirty)
	if err := runWith(t, runner, cfg, runOpts{push: true, deploy: true, revision: dirty, deployDirty: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sshCalls(runner.calls); len(got) != 1 || !strings.Contains(got[0], "ghcr.io/o/be:"+dirty) {
		t.Errorf("ssh = %q, want a deploy of the dirty tag", got)
	}
}

func TestDeployNotReachedWhenPushFails(t *testing.T) {
	runner := &fakeRunner{failOn: "docker push ghcr.io/o/fe"}
	cfg := deployConfig(t, &config.Deploy{Host: "vps"})
	if err := runWith(t, runner, cfg, deployAll); err == nil {
		t.Fatal("Run succeeded, want push failure")
	}
	if got := sshCalls(runner.calls); len(got) != 0 {
		t.Errorf("deployed after a failed push: %q", got)
	}
}

func TestDeployFailureFailsTheRun(t *testing.T) {
	runner := &fakeRunner{failOn: "ssh "}
	cfg := deployConfig(t, &config.Deploy{Host: "vps"})
	err := runWith(t, runner, cfg, deployAll)
	if err == nil || !strings.Contains(err.Error(), "deploy to vps failed") {
		t.Fatalf("err = %v, want a deploy failure", err)
	}
}

func TestShellQuote(t *testing.T) {
	if got, want := shellQuote(`it's`), `'it'\''s'`; got != want {
		t.Errorf("shellQuote = %s, want %s", got, want)
	}
}

func TestDeploySendsTheReleaseComposeFile(t *testing.T) {
	dir := t.TempDir()
	compose := "services:\n  backend:\n    image: ${BACKEND_IMAGE}\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.vps.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := deployConfig(t, &config.Deploy{Host: "deploy@vps", ComposeFile: "compose.vps.yml"})
	runner := &fakeRunner{}
	var logged strings.Builder
	p := New(dir, Options{Push: true, Deploy: true, Revision: rev, Runner: runner, Logger: log.New(&logged, "", 0)})
	if err := p.Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	ssh := sshCalls(runner.calls)
	want := " --compose-b64 '" + base64.StdEncoding.EncodeToString([]byte(compose)) + "'"
	if len(ssh) != 1 || !strings.HasSuffix(ssh[0], want) {
		t.Errorf("ssh = %q, want it to end with%s", ssh, want)
	}
	// The log says what was sent rather than dumping it.
	if !strings.Contains(logged.String(), "--compose-b64 <compose.vps.yml, ") || strings.Contains(logged.String(), base64.StdEncoding.EncodeToString([]byte(compose))) {
		t.Errorf("log:\n%s", logged.String())
	}
}

func TestMissingComposeFileIsRefusedBeforeBuilding(t *testing.T) {
	cfg := deployConfig(t, &config.Deploy{Host: "deploy@vps", ComposeFile: "nope.yml"})
	runner := &fakeRunner{}
	p := New(t.TempDir(), Options{Push: true, Deploy: true, Revision: rev, Runner: runner, Logger: log.New(io.Discard, "", 0)})
	err := p.Run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "deploy.compose_file") {
		t.Fatalf("err = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("ran %q before refusing", runner.calls)
	}
}
