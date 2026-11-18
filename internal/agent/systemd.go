package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"j-cicd/internal/config"
)

// DefaultSettle is how long a restarted unit must stay up, on the same
// process, before a deploy counts it as healthy.
const DefaultSettle = 10 * time.Second

// Unit is the systemd unit that runs a files project.
type Unit struct {
	// Unit is the unit name, e.g. "mini-bot.service" (".service" is
	// assumed when there is no suffix).
	Unit string `yaml:"unit"`
	// Scope is "system" (the default) or "user" (systemctl --user, for a
	// unit of the agent's own user). Restarting a system unit as a
	// non-root agent goes through `sudo -n`, so it needs a sudoers rule
	// for exactly that command, and fails rather than prompting.
	Scope string `yaml:"scope,omitempty"`
	// Settle is how long the unit must stay active on the same main
	// process after its restart. A process that crashes on start is
	// restarted by systemd and looks "active" again a moment later, so a
	// single look proves nothing. Default DefaultSettle.
	Settle *config.Duration `yaml:"settle,omitempty"`
}

var unitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.@\\-]*$`)

func (u *Unit) validate() error {
	if !unitName.MatchString(u.Unit) {
		return fmt.Errorf("systemd.unit %q is not a unit name", u.Unit)
	}
	if !strings.Contains(u.Unit, ".") {
		u.Unit += ".service"
	}
	switch u.Scope {
	case "":
		u.Scope = "system"
	case "system", "user":
	default:
		return fmt.Errorf("systemd.scope %q must be system or user", u.Scope)
	}
	return nil
}

func (u *Unit) settle() time.Duration {
	if u.Settle != nil {
		return u.Settle.Duration
	}
	return DefaultSettle
}

// systemctl returns the argv for a systemctl command on the unit. Only
// commands that change something need privileges; for a system unit and
// a non-root agent they go through sudo -n, which fails at once instead
// of waiting for a password nobody will type.
func (a *Agent) systemctl(u *Unit, privileged bool, args ...string) (string, []string) {
	argv := []string{}
	if u.Scope == "user" {
		argv = append(argv, "--user")
	}
	argv = append(argv, args...)
	if privileged && u.Scope == "system" && !a.isRoot() {
		return "sudo", append([]string{"-n", "systemctl"}, argv...)
	}
	return "systemctl", argv
}

// unitProps reads properties of the unit with systemctl show.
func (a *Agent) unitProps(ctx context.Context, u *Unit, props ...string) (map[string]string, error) {
	args := []string{"show", u.Unit}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	name, argv := a.systemctl(u, false, args...)
	out, err := a.runner.Output(ctx, "", name, argv...)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = v
		}
	}
	return m, nil
}

// checkUnitLoaded fails unless systemd knows the unit, so a typo in the
// config is found before any file is replaced.
func (a *Agent) checkUnitLoaded(ctx context.Context, u *Unit) error {
	props, err := a.unitProps(ctx, u, "LoadState")
	if err != nil {
		return fmt.Errorf("reading unit %s: %w", u.Unit, err)
	}
	if st := props["LoadState"]; st != "loaded" {
		return fmt.Errorf("systemd has no unit %s (LoadState=%s); install it before deploying to it", u.Unit, orNone(st))
	}
	return nil
}

// restartUnit restarts the unit and waits for it to settle.
func (a *Agent) restartUnit(ctx context.Context, proj *Project) error {
	u := proj.Systemd
	name, argv := a.systemctl(u, true, "restart", u.Unit)
	a.log.Printf("%s %s", name, strings.Join(argv, " "))
	if err := a.runner.Run(ctx, "", name, argv...); err != nil {
		if name == "sudo" {
			return fmt.Errorf("restarting %s: %w (the agent restarts system units with `sudo -n systemctl restart %s`, which needs a NOPASSWD sudoers rule for exactly that command)", u.Unit, err, u.Unit)
		}
		return fmt.Errorf("restarting %s: %w", u.Unit, err)
	}
	if err := a.waitSettled(ctx, proj); err != nil {
		a.unitLog(ctx, u)
		return err
	}
	return nil
}

// waitSettled waits until the unit has been active on one main process,
// with systemd's restart counter unchanged, for the settle period. It
// fails as soon as the unit fails, stops, or is restarted by systemd
// (which is what a crash looks like under Restart=on-failure), and when
// the project's health_timeout passes first.
func (a *Agent) waitSettled(ctx context.Context, proj *Project) error {
	u := proj.Systemd
	timeout := proj.healthTimeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		upSince         time.Time
		pid, restarts   string
		settle          = u.settle()
		describeCrashed = func(why string) error {
			return fmt.Errorf("%s %s within %s of starting", u.Unit, why, settle)
		}
	)
	for {
		props, err := a.unitProps(ctx, u, "ActiveState", "SubState", "MainPID", "NRestarts", "Result")
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return fmt.Errorf("reading unit %s: %w", u.Unit, err)
		}
		active, sub := props["ActiveState"], props["SubState"]
		switch {
		case active == "failed":
			return fmt.Errorf("%s failed (result: %s)", u.Unit, orNone(props["Result"]))
		case sub == "auto-restart":
			return describeCrashed("exited and is being restarted by systemd")
		case active == "active" && upSince.IsZero():
			upSince, pid, restarts = a.now(), props["MainPID"], props["NRestarts"]
		case active == "active":
			if props["MainPID"] != pid || props["NRestarts"] != restarts {
				return describeCrashed(fmt.Sprintf("restarted (main pid %s -> %s, restarts %s -> %s)", pid, props["MainPID"], restarts, props["NRestarts"]))
			}
		case !upSince.IsZero():
			return describeCrashed("stopped (" + active + "/" + sub + ")")
		case active == "inactive":
			return fmt.Errorf("%s is inactive after its restart", u.Unit)
		}
		if !upSince.IsZero() && a.now().Sub(upSince) >= settle {
			a.log.Printf("%s has stayed up for %s (main pid %s)", u.Unit, settle, pid)
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(a.pollInterval):
			continue
		}
		break
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s did not stay up for %s within health_timeout %s", u.Unit, settle, timeout)
	}
	return ctx.Err()
}

// unitLog prints the unit's most recent journal lines, to say why it
// didn't come up. It's best effort: reading the journal may need the
// systemd-journal group.
func (a *Agent) unitLog(ctx context.Context, u *Unit) {
	args := []string{"--no-pager", "-n", "20", "-o", "short-iso"}
	if u.Scope == "user" {
		args = append(args, "--user-unit", u.Unit)
	} else {
		args = append(args, "-u", u.Unit)
	}
	a.log.Printf("last journal lines of %s:", u.Unit)
	if err := a.runner.Run(context.WithoutCancel(ctx), "", "journalctl", args...); err != nil {
		a.log.Printf("(couldn't read the journal: %v)", err)
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
