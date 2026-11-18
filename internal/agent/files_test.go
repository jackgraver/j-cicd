package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"j-cicd/internal/config"
)

// fakeSystemd stands in for systemctl, journalctl and the check command.
// Whether the unit stays up and whether check passes depend on what is
// installed at watch: contents containing "crash" crash on start (systemd
// is restarting them), "flap" is replaced by a new main process mid-way,
// and "nocheck" fail the check.
type fakeSystemd struct {
	calls     []string
	watch     string
	loadState string // default "loaded"
	restarts  int
	polls     int
}

func (f *fakeSystemd) installed() string {
	data, _ := os.ReadFile(f.watch)
	return string(data)
}

func (f *fakeSystemd) Run(ctx context.Context, dir, name string, args ...string) error {
	cmd := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, cmd)
	switch {
	case strings.Contains(cmd, "systemctl") && strings.Contains(cmd, " restart "):
		f.restarts++
		f.polls = 0
	case name == "env" && strings.Contains(f.installed(), "nocheck"):
		return errors.New("exit status 1")
	}
	return nil
}

func (f *fakeSystemd) Output(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	if strings.Contains(cmd, "LoadState") && !strings.Contains(cmd, "ActiveState") {
		ls := f.loadState
		if ls == "" {
			ls = "loaded"
		}
		return []byte("LoadState=" + ls + "\n"), nil
	}
	f.polls++
	pid := 100 + f.restarts
	switch got := f.installed(); {
	case strings.Contains(got, "crash"):
		return []byte("ActiveState=activating\nSubState=auto-restart\nMainPID=0\nNRestarts=1\nResult=exit-code\n"), nil
	case strings.Contains(got, "flap") && f.polls > 1:
		pid += 50
	}
	return []byte("ActiveState=active\nSubState=running\nMainPID=" + strconv.Itoa(pid) + "\nNRestarts=0\nResult=success\n"), nil
}

func (f *fakeSystemd) count(substr string) int {
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

type filesFixture struct {
	t     *testing.T
	cfg   *Config
	sd    *fakeSystemd
	agent *Agent
	bin   string // the installed file's path
}

// newFilesFixture is a files project "bot" installing one file into a
// temp dir, run by a system unit, with a check.
func newFilesFixture(t *testing.T) *filesFixture {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "mini-bot")
	cfg := &Config{
		StateDir: t.TempDir(),
		Projects: []Project{{
			Name:          "bot",
			HealthTimeout: &config.Duration{Duration: 2 * time.Second},
			Files:         []File{{Name: "mini-bot", Path: bin}},
			Systemd:       &Unit{Unit: "mini-bot", Settle: &config.Duration{Duration: 5 * time.Millisecond}},
			Check:         []string{bin, "--version"},
		}},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	sd := &fakeSystemd{watch: bin}
	a := New(cfg, Options{Runner: sd, Logger: log.New(io.Discard, "", 0)})
	a.pollInterval = time.Millisecond
	a.isRoot = func() bool { return false }
	return &filesFixture{t: t, cfg: cfg, sd: sd, agent: a, bin: bin}
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func tarOf(t *testing.T, files map[string]string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(body)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	return &buf
}

// deploy sends body as mini-bot built from rev.
func (fx *filesFixture) deploy(body, rev string) error {
	fx.t.Helper()
	return fx.agent.DeployFiles(context.Background(), "bot",
		[]string{"mini-bot@sha256:" + hashOf(body)}, rev, tarOf(fx.t, map[string]string{"mini-bot": body}))
}

func (fx *filesFixture) live() string {
	fx.t.Helper()
	data, err := os.ReadFile(fx.bin)
	if err != nil {
		fx.t.Fatalf("reading installed file: %v", err)
	}
	return string(data)
}

func (fx *filesFixture) state() *ServiceState {
	fx.t.Helper()
	st, err := LoadState(fx.cfg.StateDir, "bot")
	if err != nil {
		fx.t.Fatal(err)
	}
	ss := st.Services["mini-bot"]
	if ss == nil {
		fx.t.Fatal("no state for mini-bot")
	}
	return ss
}

func TestFilesFirstDeployKeepsExistingFileForRollback(t *testing.T) {
	fx := newFilesFixture(t)
	if err := os.WriteFile(fx.bin, []byte("hand installed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fx.deploy("v2", "c2"); err != nil {
		t.Fatalf("DeployFiles: %v", err)
	}
	if fx.live() != "v2" {
		t.Errorf("installed %q, want v2", fx.live())
	}
	if info, _ := os.Stat(fx.bin); info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
	ss := fx.state()
	if ss.Current != "sha256:"+hashOf("v2") || ss.CurrentTag != "c2" || ss.LastResult != resultHealthy {
		t.Errorf("current = %s %s %s", ss.Current, ss.CurrentTag, ss.LastResult)
	}
	// The file that was there is now the rollback target, and kept.
	if ss.Previous != "sha256:"+hashOf("hand installed") || ss.PreviousTag != untrackedTag {
		t.Errorf("previous = %s %s, want the hand-installed file", ss.Previous, ss.PreviousTag)
	}
	if err := fx.agent.Rollback(context.Background(), "bot"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if fx.live() != "hand installed" {
		t.Errorf("after rollback installed %q, want the original file back", fx.live())
	}
	// Non-root restarts of a system unit go through sudo -n; checks get
	// the revision.
	if fx.sd.count("sudo -n systemctl restart mini-bot.service") != 2 {
		t.Errorf("restarts: %v", fx.sd.calls)
	}
	if fx.sd.count("env J_CICD_PROJECT=bot J_CICD_REVISION=c2 "+fx.bin+" --version") != 1 {
		t.Errorf("check not run with the revision: %v", fx.sd.calls)
	}
}

func TestFilesCrashOnStartRollsBack(t *testing.T) {
	for _, bad := range []string{"crash", "flap", "nocheck"} {
		t.Run(bad, func(t *testing.T) {
			fx := newFilesFixture(t)
			if err := fx.deploy("v1", "c1"); err != nil {
				t.Fatalf("first deploy: %v", err)
			}
			err := fx.deploy("v2 "+bad, "c2")
			if err == nil || !strings.Contains(err.Error(), "rolled back to the previous release") {
				t.Fatalf("err = %v, want a rolled-back failure", err)
			}
			if fx.live() != "v1" {
				t.Errorf("installed %q after rollback, want v1", fx.live())
			}
			ss := fx.state()
			if ss.Current != "sha256:"+hashOf("v1") || ss.LastResult != resultRolledBack {
				t.Errorf("state current=%s result=%q", ss.Current, ss.LastResult)
			}
			if fx.sd.restarts != 3 {
				t.Errorf("restarts = %d, want deploy, failed deploy, rollback", fx.sd.restarts)
			}
		})
	}
}

func TestFilesCrashWithNothingToRollBackTo(t *testing.T) {
	fx := newFilesFixture(t)
	err := fx.deploy("crash", "c1")
	if err == nil || !strings.Contains(err.Error(), "ROLLBACK FAILED") || !strings.Contains(err.Error(), "left in place") {
		t.Fatalf("err = %v", err)
	}
	if fx.state().LastResult != resultNoRollback {
		t.Errorf("result = %q", fx.state().LastResult)
	}
}

func TestFilesRedeployIsNoop(t *testing.T) {
	fx := newFilesFixture(t)
	if err := fx.deploy("v1", "c1"); err != nil {
		t.Fatal(err)
	}
	if err := fx.deploy("v1", "c1"); err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	if fx.sd.restarts != 1 {
		t.Errorf("restarted %d times, want once", fx.sd.restarts)
	}
}

func TestFilesReceiveRefusesBadInput(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		ref   string
		want  string
	}{
		"hash mismatch": {map[string]string{"mini-bot": "tampered"}, "mini-bot@sha256:" + hashOf("v2"), "not the sha256"},
		"extra file":    {map[string]string{"mini-bot": "v2", "evil": "x"}, "mini-bot@sha256:" + hashOf("v2"), "isn't one of the files"},
		"not sent":      {map[string]string{}, "mini-bot@sha256:" + hashOf("v2"), "was not sent"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newFilesFixture(t)
			if err := os.WriteFile(fx.bin, []byte("v1"), 0o755); err != nil {
				t.Fatal(err)
			}
			err := fx.agent.DeployFiles(context.Background(), "bot", []string{tc.ref}, "c2", tarOf(t, tc.files))
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("err = %v, want %q and nothing changed", err, tc.want)
			}
			if fx.live() != "v1" || fx.sd.restarts != 0 {
				t.Errorf("changed things: installed %q, %d restarts", fx.live(), fx.sd.restarts)
			}
			if fx.state().LastResult != resultReceiveFailed {
				t.Errorf("result = %q", fx.state().LastResult)
			}
		})
	}
}

func TestFilesRefsAreValidated(t *testing.T) {
	fx := newFilesFixture(t)
	for ref, want := range map[string]string{
		"mini-bot:c1":                       "not name@sha256",
		"other@sha256:" + hashOf("x"):       "has no file \"other\"",
		"ghcr.io/o/x@sha256:" + hashOf("x"): "not name@sha256",
	} {
		err := fx.agent.DeployFiles(context.Background(), "bot", []string{ref}, "", nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", ref, err, want)
		}
	}
	// Kinds don't mix.
	if err := fx.agent.Deploy(context.Background(), "bot", []string{"r:t"}, nil); err == nil || !strings.Contains(err.Error(), "installs files") {
		t.Errorf("image deploy to a files project: %v", err)
	}
}

func TestFilesPreflightChangesNothing(t *testing.T) {
	t.Run("unit not loaded", func(t *testing.T) {
		fx := newFilesFixture(t)
		fx.sd.loadState = "not-found"
		err := fx.deploy("v1", "c1")
		if err == nil || !strings.Contains(err.Error(), "no unit mini-bot.service") || !strings.Contains(err.Error(), "nothing was changed") {
			t.Fatalf("err = %v", err)
		}
		if _, statErr := os.Stat(fx.bin); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("installed despite the failed preflight")
		}
	})
	t.Run("directory not writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can write anywhere")
		}
		fx := newFilesFixture(t)
		dir := filepath.Dir(fx.bin)
		os.Chmod(dir, 0o555)
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		err := fx.deploy("v1", "c1")
		if err == nil || !strings.Contains(err.Error(), "can't install into") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("not in the store", func(t *testing.T) {
		fx := newFilesFixture(t)
		err := fx.agent.DeployFiles(context.Background(), "bot", []string{"mini-bot@sha256:" + hashOf("v9")}, "", nil)
		if err == nil || !strings.Contains(err.Error(), "isn't in the agent's store") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestFilesHandEditIsKeptForRollback(t *testing.T) {
	fx := newFilesFixture(t)
	if err := fx.deploy("v1", "c1"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(fx.bin, []byte("hotfix"), 0o755) // replaced by hand
	if err := fx.deploy("v2", "c2"); err != nil {
		t.Fatal(err)
	}
	if ss := fx.state(); ss.Previous != "sha256:"+hashOf("hotfix") || ss.PreviousTag != untrackedTag {
		t.Errorf("previous = %s %s, want the hand edit", ss.Previous, ss.PreviousTag)
	}
	if err := fx.agent.Rollback(context.Background(), "bot"); err != nil {
		t.Fatal(err)
	}
	if fx.live() != "hotfix" {
		t.Errorf("rollback installed %q, want the hand edit back", fx.live())
	}
}

func TestFilesManualDeployByHashAndPruning(t *testing.T) {
	fx := newFilesFixture(t)
	for i, v := range []string{"v1", "v2", "v3"} {
		if err := fx.deploy(v, "c"+strconv.Itoa(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	// Only current (v3) and previous (v2) are kept.
	stored := func(body string) bool {
		_, err := os.Stat(blobPath(fx.cfg.StateDir, "bot", hashOf(body), "mini-bot"))
		return err == nil
	}
	if stored("v1") || !stored("v2") || !stored("v3") {
		t.Errorf("stored v1/v2/v3 = %v/%v/%v, want false/true/true", stored("v1"), stored("v2"), stored("v3"))
	}
	// A stored release can be deployed by hash, with nothing sent.
	if err := fx.agent.DeployFiles(context.Background(), "bot", []string{"mini-bot@sha256:" + hashOf("v2")}, "", nil); err != nil {
		t.Fatalf("deploy by hash: %v", err)
	}
	if fx.live() != "v2" {
		t.Errorf("installed %q, want v2", fx.live())
	}
}

func TestFilesCorruptStoreIsCaughtBeforeInstall(t *testing.T) {
	fx := newFilesFixture(t)
	if err := fx.deploy("v1", "c1"); err != nil {
		t.Fatal(err)
	}
	if err := fx.deploy("v2", "c2"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(blobPath(fx.cfg.StateDir, "bot", hashOf("v1"), "mini-bot"), []byte("bitrot"), 0o600)
	err := fx.agent.Rollback(context.Background(), "bot")
	if err == nil || !strings.Contains(err.Error(), "corrupt") || !strings.Contains(err.Error(), "nothing was restarted") {
		t.Fatalf("err = %v", err)
	}
	if fx.live() != "v2" {
		t.Errorf("installed %q, want v2 untouched", fx.live())
	}
}

func TestFilesUserUnitAndRootAgentSkipSudo(t *testing.T) {
	fx := newFilesFixture(t)
	fx.cfg.Projects[0].Systemd.Scope = "user"
	if err := fx.deploy("v1", "c1"); err != nil {
		t.Fatal(err)
	}
	if fx.sd.count("systemctl --user restart mini-bot.service") != 1 || fx.sd.count("sudo") != 0 {
		t.Errorf("user unit: %v", fx.sd.calls)
	}

	fx = newFilesFixture(t)
	fx.agent.isRoot = func() bool { return true }
	if err := fx.deploy("v1", "c1"); err != nil {
		t.Fatal(err)
	}
	if fx.sd.count("sudo") != 0 || fx.sd.count("systemctl restart mini-bot.service") != 1 {
		t.Errorf("root agent: %v", fx.sd.calls)
	}
}

func TestFilesProjectWithoutUnitJustInstalls(t *testing.T) {
	fx := newFilesFixture(t)
	fx.cfg.Projects[0].Systemd = nil
	if err := fx.deploy("v1", "c1"); err != nil {
		t.Fatal(err)
	}
	if fx.sd.count("systemctl") != 0 || fx.live() != "v1" {
		t.Errorf("calls %v, installed %q", fx.sd.calls, fx.live())
	}
}

func TestFilesConfigValidation(t *testing.T) {
	base := func() Project {
		return Project{Name: "p", Files: []File{{Name: "bin", Path: "/opt/p/bin"}}}
	}
	cases := map[string]struct {
		mutate func(*Project)
		want   string
	}{
		"relative path":  {func(p *Project) { p.Files[0].Path = "bin" }, "absolute"},
		"unclean path":   {func(p *Project) { p.Files[0].Path = "/opt/../bin" }, "clean absolute"},
		"bad name":       {func(p *Project) { p.Files[0].Name = "a/b" }, "letters"},
		"dup path":       {func(p *Project) { p.Files = append(p.Files, File{Name: "x", Path: "/opt/p/bin"}) }, "two files"},
		"bad mode":       {func(p *Project) { p.Files[0].Mode = "rwx" }, "octal"},
		"setuid":         {func(p *Project) { p.Files[0].Mode = "4755" }, "setuid"},
		"compose too":    {func(p *Project) { p.ComposeFile = "/c.yml" }, "not both"},
		"pre_swap":       {func(p *Project) { p.PreSwap = &PreSwap{} }, "pre_swap"},
		"empty check":    {func(p *Project) { p.Check = []string{} }, "needs a command"},
		"bad unit":       {func(p *Project) { p.Systemd = &Unit{Unit: "-x"} }, "not a unit name"},
		"bad scope":      {func(p *Project) { p.Systemd = &Unit{Unit: "x", Scope: "global"} }, "system or user"},
		"check on compo": {func(p *Project) { p.Files = nil; p.Check = []string{"x"} }, "go with files"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := base()
			tc.mutate(&p)
			cfg := &Config{Projects: []Project{p}}
			if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	p := base()
	p.Files[0].Mode = "0750"
	cfg := &Config{Projects: []Project{p}}
	if err := cfg.validate(); err != nil || cfg.Projects[0].Files[0].mode != 0o750 {
		t.Errorf("mode 0750: err %v, mode %v", err, cfg.Projects[0].Files[0].mode)
	}
}
