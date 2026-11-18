package pipeline

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"j-cicd/internal/config"
)

// artifactProject writes the files an artifacts config expects and loads
// the config, deploying with d.
func artifactProject(t *testing.T, files map[string]string, d *config.Deploy) (string, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	for path, body := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := writeAndLoad(t, &config.Config{
		Name:  "tool",
		Steps: []config.Step{{Name: "build", Run: "go build -o bin/ ./..."}},
		Artifacts: []config.Artifact{
			{Name: "tool", Path: "bin/tool"},
			{Name: "tool-agent", Path: "bin/tool-agent"},
		},
		Deploy: d,
	}, rev)
	return dir, cfg
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func runIn(t *testing.T, dir string, runner *fakeRunner, cfg *config.Config, o runOpts) (*Pipeline, error) {
	t.Helper()
	p := New(dir, Options{
		Push: o.push, Deploy: o.deploy, Revision: o.revision, DeployDirty: o.deployDirty,
		Runner: runner, Logger: log.New(io.Discard, "", 0),
	})
	return p, p.Run(t.Context(), cfg)
}

func TestArtifactsDeploySendsFilesByHash(t *testing.T) {
	files := map[string]string{"bin/tool": "tool v2", "bin/tool-agent": "agent v2"}
	dir, cfg := artifactProject(t, files, &config.Deploy{Host: "vps"})
	runner := &fakeRunner{}
	p, err := runIn(t, dir, runner, cfg, deployAll)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := "ssh -o BatchMode=yes -- vps j-cicd-agent deploy 'tool' 'tool@sha256:" + sha("tool v2") +
		"' 'tool-agent@sha256:" + sha("agent v2") + "' '--artifacts-stdin' '--revision' '" + rev + "'"
	if last := runner.calls[len(runner.calls)-1]; last != want {
		t.Errorf("deploy command:\n got %q\nwant %q", last, want)
	}
	// The stdin is a tar with each file under its artifact name.
	got := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(runner.stdin))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading sent tar: %v", err)
		}
		body, _ := io.ReadAll(tr)
		got[h.Name] = string(body)
	}
	if got["tool"] != "tool v2" || got["tool-agent"] != "agent v2" || len(got) != 2 {
		t.Errorf("sent %v", got)
	}
	if arts := p.Artifacts(); len(arts) != 2 || arts[0].SHA256 != sha("tool v2") || arts[0].Size != 7 {
		t.Errorf("Artifacts() = %+v", arts)
	}
	if !p.Deployed() {
		t.Error("Deployed() = false")
	}
}

func TestArtifactsDeployLocalRunsAgentThroughSh(t *testing.T) {
	files := map[string]string{"bin/tool": "a", "bin/tool-agent": "b"}
	dir, cfg := artifactProject(t, files, &config.Deploy{Local: true, Command: "J_CICD_AGENT_CONFIG=$HOME/a.yaml j-cicd-agent", Artifacts: []string{"tool"}})
	runner := &fakeRunner{}
	if _, err := runIn(t, dir, runner, cfg, deployAll); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := "sh -c J_CICD_AGENT_CONFIG=$HOME/a.yaml j-cicd-agent deploy 'tool' 'tool@sha256:" + sha("a") + "' '--artifacts-stdin' '--revision' '" + rev + "'"
	if last := runner.calls[len(runner.calls)-1]; last != want {
		t.Errorf("deploy command:\n got %q\nwant %q", last, want)
	}
	if len(sshCalls(runner.calls)) != 0 {
		t.Errorf("a local deploy ran ssh: %v", runner.calls)
	}
}

func TestMissingArtifactFailsBeforeDeploy(t *testing.T) {
	dir, cfg := artifactProject(t, map[string]string{"bin/tool": "a"}, &config.Deploy{Host: "vps"})
	runner := &fakeRunner{}
	_, err := runIn(t, dir, runner, cfg, deployAll)
	if err == nil || !strings.Contains(err.Error(), "artifact tool-agent: the steps didn't produce bin/tool-agent") {
		t.Fatalf("err = %v, want the missing artifact named", err)
	}
	if len(sshCalls(runner.calls)) != 0 {
		t.Errorf("deployed despite a missing artifact: %v", runner.calls)
	}
}

func TestArtifactsDeployRefusesDirtyTreeBeforeSteps(t *testing.T) {
	dir, cfg := artifactProject(t, map[string]string{"bin/tool": "a", "bin/tool-agent": "b"}, &config.Deploy{Host: "vps"})
	runner := &fakeRunner{}
	_, err := runIn(t, dir, runner, cfg, runOpts{push: true, deploy: true, revision: rev + "-dirty"})
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("err = %v, want the dirty tree refused", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("ran %v before refusing", runner.calls)
	}
}

func TestArtifactsBuildOnlyChecksButDoesNotDeploy(t *testing.T) {
	dir, cfg := artifactProject(t, map[string]string{"bin/tool": "a", "bin/tool-agent": "b"}, &config.Deploy{Host: "vps"})
	runner := &fakeRunner{}
	p, err := runIn(t, dir, runner, cfg, runOpts{push: false, deploy: true, revision: rev})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sshCalls(runner.calls)) != 0 || p.Deployed() {
		t.Errorf("--no-push deployed: %v", runner.calls)
	}
	if len(p.Artifacts()) != 2 {
		t.Errorf("artifacts not checked on a build-only run: %+v", p.Artifacts())
	}
}

func TestRollbackLocal(t *testing.T) {
	_, cfg := artifactProject(t, nil, &config.Deploy{Local: true})
	runner := &fakeRunner{}
	p := New("/proj", Options{Runner: runner, Logger: log.New(io.Discard, "", 0)})
	if err := p.Rollback(t.Context(), cfg); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if want := "sh -c j-cicd-agent rollback 'tool'"; runner.calls[0] != want {
		t.Errorf("rollback command = %q, want %q", runner.calls[0], want)
	}
}
