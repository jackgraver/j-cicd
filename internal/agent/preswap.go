package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"j-cicd/internal/config"
)

// PreSwap is a command run inside the new image of one service, after
// pre_deploy and before any container is replaced — e.g. a database
// migration shipped in the image. The agent decides when it runs; what it
// does is the application's business. A non-zero exit or a timeout stops
// the deploy with nothing changed. It is never undone: a rollback puts
// images back, not whatever the command did.
type PreSwap struct {
	// Service is the compose service whose new image runs the command,
	// with the same compose file, env file and environment as the deploy
	// itself. It must be one of the project's services:.
	Service string `yaml:"service"`
	// Command is the command and its arguments, run as-is (no shell).
	Command []string `yaml:"command"`
	// Timeout bounds the command. Unset or 0, only the deploy's own
	// timeout does.
	Timeout *config.Duration `yaml:"timeout,omitempty"`
}

func (h *PreSwap) validate(p *Project) error {
	if h.Service == "" {
		return errors.New("pre_swap.service is required")
	}
	if !slices.ContainsFunc(p.Services, func(s Service) bool { return s.Name == h.Service }) {
		// Its image_var is how the new image reaches `compose run`.
		return fmt.Errorf("pre_swap.service %q must be one of the project's services:", h.Service)
	}
	if len(h.Command) == 0 || h.Command[0] == "" {
		return errors.New("pre_swap.command is required")
	}
	return nil
}

// preSwap runs the project's pre_swap command, if it has one and its
// service is among those about to change, as a one-off container of the
// new release. Its output streams into the deploy's like any other
// command's.
func (a *Agent) preSwap(ctx context.Context, proj *Project, st *State, changed []release) error {
	h := proj.PreSwap
	if h == nil {
		return nil
	}
	i := slices.IndexFunc(changed, func(r release) bool { return r.svc.Name == h.Service })
	if i < 0 {
		a.log.Printf("pre_swap: skipped, %s is not changing", h.Service)
		return nil
	}

	hctx, cancel := ctx, context.CancelFunc(func() {})
	if h.Timeout != nil && h.Timeout.Duration > 0 {
		hctx, cancel = context.WithTimeout(ctx, h.Timeout.Duration)
	}
	defer cancel()

	// Named, so it can be found and removed if it has to be stopped.
	name := proj.Name + "-pre-swap-" + a.now().UTC().Format("20060102-150405")
	env := imageEnv(proj, st, changed, func(r release) string { return r.digest })
	// --no-deps: like the deploy's `up`, never start or recreate anything
	// else (a dependency the new compose file changed, say). A database
	// that isn't running makes the command fail, which stops the deploy.
	// -T: there is no terminal on the other end of the ssh session.
	args := append([]string{"run", "--rm", "--no-deps", "-T", "--name", name, h.Service}, h.Command...)
	what := strings.Join(h.Command, " ")
	a.log.Printf("pre_swap: running %q in %s %s", what, h.Service, label(changed[i]))
	err := a.compose(hctx, proj, env, args...)
	if err == nil {
		a.log.Printf("pre_swap: %q succeeded", what)
		return nil
	}
	if hctx.Err() != nil {
		a.stopPreSwap(context.WithoutCancel(ctx), name)
	}
	switch {
	case ctx.Err() != nil: // the whole deploy was cancelled or ran out of time
		return fmt.Errorf("%q: %w", what, ctx.Err())
	case hctx.Err() != nil:
		return fmt.Errorf("%q timed out after %s", what, h.Timeout.Duration)
	default:
		return fmt.Errorf("%q: %w", what, err)
	}
}

// preSwapStopGrace is how long a timed-out pre_swap command gets to exit
// after SIGTERM before it is killed.
const preSwapStopGrace = 30 * time.Second

// stopPreSwap stops a pre_swap container the deploy gave up on. Killing
// the compose client doesn't stop its container, so the command would
// otherwise carry on (and keep changing things) unobserved.
//
// SIGTERM first, with a grace period: SIGKILL alone isn't enough to stop
// the work. A database never hears about a client that died mid-statement
// until it next writes to the connection, so a killed migration keeps
// running in Postgres, holding its locks, until that statement ends. A
// command that handles SIGTERM can cancel the statement itself (pgx sends
// a cancel request when its context is cancelled). The container is
// removed by the daemon once stopped (run --rm), so rm -f is only the
// fallback if the stop fails.
func (a *Agent) stopPreSwap(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(ctx, preSwapStopGrace+30*time.Second)
	defer cancel()
	a.log.Printf("pre_swap: stopping %s (SIGTERM, then SIGKILL after %s)", name, preSwapStopGrace)
	stopErr := a.runner.Run(ctx, "", "docker", "stop", "-t", fmt.Sprint(int(preSwapStopGrace.Seconds())), name)
	if stopErr == nil {
		return
	}
	if rmErr := a.runner.Run(ctx, "", "docker", "rm", "-f", name); rmErr != nil {
		a.log.Printf("warning: stopping pre_swap container %s: %v; removing it: %v (check `docker ps`)", name, stopErr, rmErr)
	}
}
