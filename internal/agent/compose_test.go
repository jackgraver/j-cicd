package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func composeHash(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// usesCompose reports whether a compose command ran the release compose
// file with the given contents, from the configured project directory.
func usesCompose(cfg *Config, cmd, data string) bool {
	return strings.Contains(cmd, " docker compose --project-directory /opt/st -f "+composePath(cfg.StateDir, "st", composeHash(data))+" ")
}

func deploy(t *testing.T, a *Agent, refs []string, compose string) {
	t.Helper()
	var c []byte
	if compose != "" {
		c = []byte(compose)
	}
	if err := a.Deploy(context.Background(), "st", refs, c); err != nil {
		t.Fatalf("Deploy(%v, %q): %v", refs, compose, err)
	}
}

func TestComposeSentWithDeployIsStoredAndUsed(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{}
	a := testAgent(cfg, docker)
	deploy(t, a, []string{beV1, feV1}, "services: v1\n")

	ups := docker.composeCalls("up")
	if len(ups) != 1 || !usesCompose(cfg, ups[0], "services: v1\n") {
		t.Errorf("up = %q", ups)
	}
	if data, err := os.ReadFile(composePath(cfg.StateDir, "st", composeHash("services: v1\n"))); err != nil || string(data) != "services: v1\n" {
		t.Errorf("stored compose = %q, %v", data, err)
	}
	if c := state(t, cfg).Compose; c == nil || c.Current != composeHash("services: v1\n") || c.Previous != "" {
		t.Errorf("compose state = %+v", c)
	}
}

func TestComposeOnlyChangeAppliesToEveryDeployedService(t *testing.T) {
	// The bug this exists for: a release that only changes the compose
	// file (say, a new environment variable) has the same image digests,
	// and used to be skipped as "already running".
	cfg, docker := testConfig(t), &fakeDocker{}
	a := testAgent(cfg, docker)
	deploy(t, a, []string{beV1, feV1}, "v1")
	docker.calls = nil

	deploy(t, a, []string{beV1}, "v2") // backend only, same digest
	ups := docker.composeCalls("up")
	if len(ups) != 1 || !usesCompose(cfg, ups[0], "v2") || !strings.HasSuffix(ups[0], "up -d --no-deps backend frontend") {
		t.Errorf("up = %q, want both services on the new compose file", ups)
	}
	if c := state(t, cfg).Compose; c.Current != composeHash("v2") || c.Previous != composeHash("v1") {
		t.Errorf("compose state = %+v", c)
	}
}

func TestSameComposeAndImagesIsANoop(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{}
	a := testAgent(cfg, docker)
	deploy(t, a, []string{beV1, feV1}, "v1")
	docker.calls = nil
	deploy(t, a, []string{beV1, feV1}, "v1")
	if ups := docker.composeCalls("up"); len(ups) != 0 {
		t.Errorf("up = %q, want nothing to do", ups)
	}
}

func TestDeployWithoutComposeKeepsTheCurrentOne(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{}
	a := testAgent(cfg, docker)
	deploy(t, a, []string{beV1}, "v1")
	docker.calls = nil
	deploy(t, a, []string{beV2}, "")
	if ups := docker.composeCalls("up"); len(ups) != 1 || !usesCompose(cfg, ups[0], "v1") {
		t.Errorf("up = %q, want the stored compose file", ups)
	}
}

func TestFailedDeployRollsBackOntoTheOldComposeFile(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{health: map[string]string{beV2Digest: "running unhealthy"}}
	a := testAgent(cfg, docker)
	deploy(t, a, []string{beV1}, "v1")
	docker.calls = nil

	if err := a.Deploy(context.Background(), "st", []string{beV2}, []byte("v2")); err == nil {
		t.Fatal("want the unhealthy deploy to fail")
	}
	ups := docker.composeCalls("up")
	if len(ups) != 2 || !usesCompose(cfg, ups[0], "v2") || !usesCompose(cfg, ups[1], "v1") {
		t.Errorf("up = %q, want the release on v2 then the rollback on v1", ups)
	}
	if c := state(t, cfg).Compose; c.Current != composeHash("v1") {
		t.Errorf("compose state = %+v, want v1 still current", c)
	}
}

func TestRollbackRestoresTheComposeFile(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{}
	a := testAgent(cfg, docker)
	deploy(t, a, []string{beV1, feV1}, "v1")
	deploy(t, a, []string{beV2, feV1}, "v2")
	docker.calls = nil

	if err := a.Rollback(context.Background(), "st"); err != nil {
		t.Fatal(err)
	}
	ups := docker.composeCalls("up")
	if len(ups) != 1 || !usesCompose(cfg, ups[0], "v1") {
		t.Errorf("up = %q, want the previous compose file", ups)
	}
	if got := docker.running["BACKEND_IMAGE"]; got != beV1Digest {
		t.Errorf("backend = %s, want %s", got, beV1Digest)
	}
	if c := state(t, cfg).Compose; c.Current != composeHash("v1") || c.Previous != composeHash("v2") {
		t.Errorf("compose state = %+v", c)
	}
}

func TestRollbackKeepsComposeTheLastDeployDidntChange(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{}
	a := testAgent(cfg, docker)
	deploy(t, a, []string{beV1}, "v1")
	deploy(t, a, []string{beV2}, "v1") // images only
	docker.calls = nil

	if err := a.Rollback(context.Background(), "st"); err != nil {
		t.Fatal(err)
	}
	if ups := docker.composeCalls("up"); len(ups) != 1 || !usesCompose(cfg, ups[0], "v1") {
		t.Errorf("up = %q, want v1 kept", ups)
	}
}

func TestOldComposeFilesArePruned(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{}
	a := testAgent(cfg, docker)
	for _, c := range []string{"v1", "v2", "v3"} {
		deploy(t, a, []string{beV1}, c)
	}
	entries, err := os.ReadDir(composeDir(cfg.StateDir, "st"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 || !strings.Contains(strings.Join(names, " "), composeHash("v3")) || !strings.Contains(strings.Join(names, " "), composeHash("v2")) {
		t.Errorf("kept %v, want only v2 and v3 (current and previous)", names)
	}
}
