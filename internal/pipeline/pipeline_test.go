package pipeline

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"j-cicd/internal/config"
)

// fakeRunner records the commands it is asked to run instead of running
// them, and can fail on the nth call.
type fakeRunner struct {
	calls   []string
	failOn  string // substring of the command that should fail
	err     error
	block   bool // honor ctx instead of returning immediately
	lastDir string
	// inspected are the image refs whose pushed digest was read back.
	inspected []string
	// stdin is what RunInput was fed, read in full.
	stdin []byte
}

// RunInput records the call like Run, and reads stdin to the end the way
// the agent would.
func (f *fakeRunner) RunInput(ctx context.Context, stdin io.Reader, dir, name string, args ...string) error {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	f.stdin = data
	return f.Run(ctx, dir, name, args...)
}

func (f *fakeRunner) Run(ctx context.Context, dir, name string, args ...string) error {
	cmd := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, cmd)
	f.lastDir = dir
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.failOn != "" && strings.Contains(cmd, f.failOn) {
		if f.err != nil {
			return f.err
		}
		return errors.New("exit status 1")
	}
	return nil
}

// Output records the call like Run and returns no output, except that
// reading back a pushed digest (docker image inspect) is answered with a
// digest and kept out of calls, which tests use to assert the build and
// push sequence.
func (f *fakeRunner) Output(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	if name == "docker" && len(args) > 1 && args[0] == "image" && args[1] == "inspect" {
		repo, _, _ := strings.Cut(args[len(args)-1], ":")
		f.inspected = append(f.inspected, args[len(args)-1])
		return []byte(repo + "@sha256:" + strings.Repeat("ab", 32) + "\n"), nil
	}
	return nil, f.Run(ctx, dir, name, args...)
}

func testPipeline(projectDir string, runner CommandRunner, push bool) *Pipeline {
	return New(projectDir, Options{
		Push:   push,
		Runner: runner,
		Logger: log.New(io.Discard, "", 0),
	})
}

func loaded(t *testing.T, cfg *config.Config) *config.Config {
	t.Helper()
	// Defaults (dockerfile, context, tags, step names) are applied by the
	// loader, so tests go through it rather than hand-filling fields.
	return writeAndLoad(t, cfg, "")
}

func TestRunStepsInOrderThenImages(t *testing.T) {
	runner := &fakeRunner{}
	cfg := loaded(t, &config.Config{
		Name: "demo",
		Steps: []config.Step{
			{Name: "build", Run: "make build"},
			{Name: "test", Run: "make test"},
		},
		Images: []config.Image{
			{Repository: "example.com/demo", Tags: []string{"latest"}},
		},
	})

	if err := testPipeline("/proj", runner, true).Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{
		dockerCheck,
		"sh -c make build",
		"sh -c make test",
		"docker build -f /proj/Dockerfile -t example.com/demo:latest /proj",
		"docker push example.com/demo:latest",
	}
	assertCalls(t, runner.calls, want)
}

func TestRunStopsAtFirstFailedStep(t *testing.T) {
	runner := &fakeRunner{failOn: "make test"}
	cfg := loaded(t, &config.Config{
		Name: "demo",
		Steps: []config.Step{
			{Name: "build", Run: "make build"},
			{Name: "test", Run: "make test"},
			{Name: "lint", Run: "make lint"},
		},
		Images: []config.Image{{Repository: "example.com/demo"}},
	})

	err := testPipeline("/proj", runner, true).Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run succeeded, want failure")
	}
	if !strings.Contains(err.Error(), `step "test" failed`) {
		t.Errorf("error = %v, want it to name the failing step", err)
	}
	// Nothing after the failure may run — especially not a push.
	assertCalls(t, runner.calls, []string{dockerCheck, "sh -c make build", "sh -c make test"})
}

func TestRunNoPushBuildsButDoesNotPush(t *testing.T) {
	runner := &fakeRunner{}
	cfg := loaded(t, &config.Config{
		Name:   "demo",
		Images: []config.Image{{Repository: "example.com/demo", Tags: []string{"latest", "abc123"}}},
	})

	if err := testPipeline("/proj", runner, false).Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "push") {
			t.Errorf("pushed despite --no-push: %q", call)
		}
	}
	assertCalls(t, runner.calls, []string{
		dockerCheck,
		"docker build -f /proj/Dockerfile -t example.com/demo:latest -t example.com/demo:abc123 /proj",
	})
}

func TestRunPushesEveryTagOfEveryImage(t *testing.T) {
	runner := &fakeRunner{}
	cfg := loaded(t, &config.Config{
		Name: "multi",
		Images: []config.Image{
			{Name: "backend", Repository: "example.com/be", Dockerfile: "backend/Dockerfile", Context: "backend", Tags: []string{"latest", "abc123"}},
			{Name: "frontend", Repository: "example.com/fe", Dockerfile: "frontend/Dockerfile", Context: "frontend"},
		},
	})

	if err := testPipeline("/proj", runner, true).Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertCalls(t, runner.calls, []string{
		dockerCheck,
		"docker build -f /proj/backend/Dockerfile -t example.com/be:latest -t example.com/be:abc123 /proj/backend",
		"docker push example.com/be:latest",
		"docker push example.com/be:abc123",
		"docker build -f /proj/frontend/Dockerfile -t example.com/fe:latest /proj/frontend",
		"docker push example.com/fe:latest",
	})
}

func TestRunBuildFailureSkipsPush(t *testing.T) {
	runner := &fakeRunner{failOn: "docker build"}
	cfg := loaded(t, &config.Config{
		Name:   "demo",
		Images: []config.Image{{Repository: "example.com/demo"}},
	})

	err := testPipeline("/proj", runner, true).Run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "docker build") {
		t.Fatalf("err = %v, want a docker build failure", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "push") {
			t.Errorf("pushed after a failed build: %q", call)
		}
	}
}

func TestRunStepDirIsRelativeToProject(t *testing.T) {
	runner := &fakeRunner{}
	cfg := loaded(t, &config.Config{
		Name:  "demo",
		Steps: []config.Step{{Name: "test", Run: "npm test", Dir: "frontend"}},
	})

	if err := testPipeline("/proj", runner, true).Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.lastDir != "/proj/frontend" {
		t.Errorf("step dir = %q, want /proj/frontend", runner.lastDir)
	}
}

func TestRunStepTimeoutIsReported(t *testing.T) {
	runner := &fakeRunner{block: true}
	cfg := loaded(t, &config.Config{
		Name:  "demo",
		Steps: []config.Step{{Name: "hang", Run: "sleep forever", Timeout: &config.Duration{Duration: 10 * time.Millisecond}}},
	})

	err := testPipeline("/proj", runner, true).Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run succeeded, want a timeout")
	}
	if !strings.Contains(err.Error(), "timed out after 10ms") {
		t.Errorf("error = %v, want it to report the timeout", err)
	}
}

func TestRunCancelledContextStopsBeforeNextStep(t *testing.T) {
	runner := &fakeRunner{}
	cfg := loaded(t, &config.Config{
		Name:  "demo",
		Steps: []config.Step{{Name: "build", Run: "make build"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := testPipeline("/proj", runner, true).Run(ctx, cfg)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("ran %v after cancellation, want nothing", runner.calls)
	}
}

func assertCalls(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("commands:\n got %q\nwant %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

func TestRunReadsBackPushedDigests(t *testing.T) {
	runner := &fakeRunner{}
	cfg := loaded(t, &config.Config{Name: "p", Images: []config.Image{{Name: "web", Repository: "example.com/web", Tags: []string{"latest", "abc"}}}})
	p := testPipeline("/proj", runner, true)
	if err := p.Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	imgs := p.Images()
	if len(imgs) != 1 || imgs[0].Digest != "sha256:"+strings.Repeat("ab", 32) || imgs[0].Repository != "example.com/web" {
		t.Errorf("Images = %+v", imgs)
	}
	if p.Deployed() {
		t.Error("Deployed = true without a deploy: section")
	}
}

func TestNoPushNoDigests(t *testing.T) {
	runner := &fakeRunner{}
	cfg := loaded(t, &config.Config{Name: "p", Images: []config.Image{{Repository: "example.com/web"}}})
	p := testPipeline("/proj", runner, false)
	if err := p.Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(p.Images()) != 0 || len(runner.inspected) != 0 {
		t.Errorf("a build without push reported pushed images: %+v", p.Images())
	}
}

func TestRunProgressLines(t *testing.T) {
	var progress strings.Builder
	cfg := deployConfig(t, &config.Deploy{Host: "vps"})
	p := New("/proj", Options{Push: true, Deploy: true, Revision: rev, Runner: &fakeRunner{}, Logger: log.New(io.Discard, "", 0), Progress: &progress})
	if err := p.Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	want := "• step test\n• building image backend\n• pushing image backend\n• building image frontend\n• pushing image frontend\n• deploying to vps\n"
	if progress.String() != want {
		t.Errorf("progress:\n got %q\nwant %q", progress.String(), want)
	}
	if !p.Deployed() {
		t.Error("Deployed = false after a deploy")
	}
}

func TestPlan(t *testing.T) {
	cfg := deployConfig(t, &config.Deploy{Host: "deploy@vps"})
	runner := &fakeRunner{}
	p := New("/proj", Options{Push: true, Deploy: true, Revision: rev, Runner: runner})
	plan := p.Plan(cfg)
	if len(plan.Steps) != 1 || len(plan.Images) != 2 || !plan.Images[0].Push {
		t.Errorf("plan = %+v", plan)
	}
	if plan.Deploy == nil || plan.Deploy.Host != "deploy@vps" || len(plan.Deploy.Images) != 2 || len(plan.Problems) != 0 {
		t.Errorf("deploy = %+v, problems %v", plan.Deploy, plan.Problems)
	}
	if len(runner.calls) != 0 {
		t.Errorf("planning ran %q", runner.calls)
	}

	dirty := New("/proj", Options{Push: true, Deploy: true, Revision: rev + "-dirty"}).Plan(cfg)
	if dirty.Deploy != nil || len(dirty.Problems) != 1 || !strings.Contains(dirty.Problems[0], "uncommitted") {
		t.Errorf("dirty plan = %+v", dirty)
	}
	skip := New("/proj", Options{Push: true, Revision: rev}).Plan(cfg)
	if skip.Deploy != nil || len(skip.Skipped) != 1 || len(skip.Problems) != 0 {
		t.Errorf("--no-deploy plan = %+v", skip)
	}
}
