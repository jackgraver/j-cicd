package pipeline

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"j-cicd/internal/config"
)

// deployRefs returns what the deploy stage sends the agent: each deployed
// image as repo:<revision>, the per-commit tag this run just pushed (none
// for an artifacts deploy, whose refs are the files' hashes, known only
// once the steps have built them). The
// agent pulls it, resolves it to a digest, and runs and records that
// digest, so the tag only has to be right for the moment between this
// push and that pull.
//
// latest can't stand in: by the time anyone rolls back it has moved. So a
// build with no per-commit tag (no git, or explicit tags: that leave the
// revision out) can't be deployed, and neither can a build of uncommitted
// changes unless DeployDirty says so.
func (p *Pipeline) deployRefs(cfg *config.Config) ([]string, error) {
	if p.revision == "" {
		return nil, errors.New("no git revision to tag it with (is the project a git repository with at least one commit?)")
	}
	if strings.HasSuffix(p.revision, "-dirty") && !p.deployDirty {
		return nil, fmt.Errorf("the tree has uncommitted changes (revision %s); commit them, or pass --deploy-dirty to deploy them anyway", p.revision)
	}
	if f := cfg.Deploy.ComposeFile; f != "" {
		if _, err := os.Stat(filepath.Join(p.projectDir, f)); err != nil {
			return nil, fmt.Errorf("deploy.compose_file: %w", err)
		}
	}
	if cfg.Deploy.SendsArtifacts() {
		return nil, nil
	}
	var refs []string
	for _, img := range cfg.DeployImages() {
		if !slices.Contains(img.Tags, p.revision) {
			return nil, fmt.Errorf("image %s isn't tagged %q: deploy needs the per-commit tag, so drop tags: from it (or include the revision)", img.Label(), p.revision)
		}
		refs = append(refs, ref(img, p.revision))
	}
	return refs, nil
}

// runDeploy asks the agent on the deploy host to roll out refs (each a
// repo:<revision> that was just pushed), or, for an artifacts deploy, the
// artifacts the steps built. It runs over plain ssh, so whatever keys and
// ~/.ssh/config the build host already has are the credentials, and the
// VPS needs no open port of its own (a local deploy runs the agent
// directly). The agent's output streams back into this run's log, and its
// exit status decides whether the run passed: a deploy that failed health
// checks (and was rolled back) fails the pipeline.
func (p *Pipeline) runDeploy(ctx context.Context, cfg *config.Config, refs []string) error {
	if cfg.Deploy.SendsArtifacts() {
		return p.deployArtifacts(ctx, cfg)
	}
	name, args := agentCommand(cfg.Deploy, "deploy", refs...)
	shown := strings.Join(args, " ")
	if f := cfg.Deploy.ComposeFile; f != "" {
		data, err := os.ReadFile(filepath.Join(p.projectDir, f))
		if err != nil {
			return fmt.Errorf("reading deploy.compose_file: %w", err)
		}
		if len(data) > maxComposeFile {
			return fmt.Errorf("deploy.compose_file %s is %d KiB; the most that fits in the ssh command is %d KiB", f, len(data)>>10, maxComposeFile>>10)
		}
		// It travels inside the ssh command line, so base64 keeps it one
		// shell-safe word. The log gets a description, not the blob.
		encoded := base64.StdEncoding.EncodeToString(data)
		args[len(args)-1] += " --compose-b64 " + shellQuote(encoded)
		shown += fmt.Sprintf(" --compose-b64 <%s, %d bytes>", f, len(data))
	}
	p.log.Printf("[deploy] %s %s", name, shown)
	return p.exec(ctx, cfg.DeployTimeout(DefaultTimeout), "", name, args...)
}

// deployArtifacts streams the deployed artifacts to the agent as a tar on
// stdin, naming each by its sha256, along with the revision they were
// built from. The agent verifies every file against its hash before it
// installs anything.
func (p *Pipeline) deployArtifacts(ctx context.Context, cfg *config.Config) error {
	in, ok := p.runner.(InputRunner)
	if !ok {
		return errors.New("this runner can't send artifacts (it has no stdin support)")
	}
	arts := p.deployedArtifacts(cfg)
	refs := make([]string, len(arts))
	for i, a := range arts {
		refs[i] = a.Ref()
	}
	extra := []string{"--artifacts-stdin"}
	if p.revision != "" {
		extra = append(extra, "--revision", p.revision)
	}
	name, args := agentCommand(cfg.Deploy, "deploy", append(refs, extra...)...)
	p.log.Printf("[deploy] %s %s  (+ %d artifact(s) on stdin)", name, strings.Join(args, " "), len(arts))
	_, err := withDeadline(ctx, cfg.DeployTimeout(DefaultTimeout), func(ctx context.Context) ([]byte, error) {
		stream := artifactStream(arts)
		defer stream.Close()
		return nil, in.RunInput(ctx, stream, "", name, args...)
	})
	return err
}

// maxComposeFile matches the agent's limit on a compose file sent with a
// deploy (the kernel caps one argument at 128 KiB, and base64 adds a
// third).
const maxComposeFile = 64 << 10

// Rollback asks the agent on the deploy host to revert the project's last
// deploy, over the same ssh path as a deploy. The agent decides what that
// means (it alone knows what was deployed) and health-checks the result;
// its exit status is the rollback's.
func (p *Pipeline) Rollback(ctx context.Context, cfg *config.Config) error {
	if cfg.Deploy == nil {
		return fmt.Errorf("%q has no deploy: section, so there is nothing to roll back", cfg.Name)
	}
	target := cfg.Deploy.Target()
	name, args := agentCommand(cfg.Deploy, "rollback")
	p.say("rolling back on %s", target)
	p.log.Printf("[rollback] %s %s", name, strings.Join(args, " "))
	if err := p.exec(ctx, cfg.DeployTimeout(DefaultTimeout), "", name, args...); err != nil {
		return fmt.Errorf("rollback on %s failed: %w", target, err)
	}
	return nil
}

// agentCommand builds the command that runs an agent subcommand on the
// project: over ssh, or through sh on this machine for a local deploy.
// Either way deploy.command is interpreted by a shell, unquoted, so it
// can be a snippet like "sudo j-cicd-agent" or carry env assignments;
// each agent argument is quoted (ssh joins everything after the
// destination into one string for the remote shell).
func agentCommand(d *config.Deploy, subcommand string, args ...string) (string, []string) {
	remote := []string{d.Command, subcommand, shellQuote(d.Project)}
	for _, a := range args {
		remote = append(remote, shellQuote(a))
	}
	script := strings.Join(remote, " ")
	if d.Local {
		return "sh", []string{"-c", script}
	}
	return "ssh", []string{
		// Never stop to prompt for a password or host key: there is no
		// one at the keyboard, and a prompt would hang until the timeout.
		"-o", "BatchMode=yes",
		"--", d.Host,
		script,
	}
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
