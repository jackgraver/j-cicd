package pipeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"j-cicd/internal/config"
)

// deployRefs returns what the deploy stage sends the agent: each deployed
// image as repo:<revision>, the per-commit tag this run just pushed. The
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
// repo:<revision> that was just pushed). It runs over plain ssh, so
// whatever keys and ~/.ssh/config the build host already has are the
// credentials, and the VPS needs no open port of its own. The agent's
// output streams back into this run's log, and its exit status decides
// whether the run passed: a deploy that failed health checks (and was
// rolled back) fails the pipeline.
func (p *Pipeline) runDeploy(ctx context.Context, cfg *config.Config, refs []string) error {
	args := sshArgs(cfg.Deploy, "deploy", refs...)
	p.log.Printf("[deploy] ssh %s", strings.Join(args, " "))
	return p.exec(ctx, cfg.DeployTimeout(DefaultTimeout), "", "ssh", args...)
}

// Rollback asks the agent on the deploy host to revert the project's last
// deploy, over the same ssh path as a deploy. The agent decides what that
// means (it alone knows what was deployed) and health-checks the result;
// its exit status is the rollback's.
func (p *Pipeline) Rollback(ctx context.Context, cfg *config.Config) error {
	if cfg.Deploy == nil {
		return fmt.Errorf("%q has no deploy: section, so there is nothing to roll back", cfg.Name)
	}
	args := sshArgs(cfg.Deploy, "rollback")
	p.log.Printf("[rollback] ssh %s", strings.Join(args, " "))
	if err := p.exec(ctx, cfg.DeployTimeout(DefaultTimeout), "", "ssh", args...); err != nil {
		return fmt.Errorf("rollback on %s failed: %w", cfg.Deploy.Host, err)
	}
	return nil
}

// sshArgs builds the ssh argv for an agent subcommand on the project. ssh
// joins everything after the destination into one string for the remote
// shell, so each agent argument is quoted; deploy.command is deliberately
// left unquoted so it can be a snippet like "sudo j-cicd-agent".
func sshArgs(d *config.Deploy, subcommand string, refs ...string) []string {
	remote := []string{d.Command, subcommand, shellQuote(d.Project)}
	for _, r := range refs {
		remote = append(remote, shellQuote(r))
	}
	return []string{
		// Never stop to prompt for a password or host key: there is no
		// one at the keyboard, and a prompt would hang until the timeout.
		"-o", "BatchMode=yes",
		"--", d.Host,
		strings.Join(remote, " "),
	}
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
