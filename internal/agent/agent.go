package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"j-cicd/internal/pipeline"
)

// Options configure an Agent.
type Options struct {
	// Runner executes docker commands. Nil means pipeline.ExecRunner —
	// tests substitute a fake.
	Runner pipeline.CommandRunner
	// Logger receives progress output. Nil means the standard logger.
	Logger *log.Logger
}

// Agent deploys releases for the projects in its config.
type Agent struct {
	cfg          *Config
	runner       pipeline.CommandRunner
	log          *log.Logger
	pollInterval time.Duration
	now          func() time.Time
	// isRoot reports whether the agent runs as root (systemd restarts
	// then need no sudo). Tests replace it.
	isRoot func() bool
}

// New returns an Agent for cfg.
func New(cfg *Config, opts Options) *Agent {
	a := &Agent{
		cfg:          cfg,
		runner:       opts.Runner,
		log:          opts.Logger,
		pollInterval: 2 * time.Second,
		now:          time.Now,
		isRoot:       func() bool { return os.Geteuid() == 0 },
	}
	if a.runner == nil {
		a.runner = pipeline.ExecRunner{}
	}
	if a.log == nil {
		a.log = log.Default()
	}
	return a
}

var (
	digestRef = regexp.MustCompile(`^([^@\s]+)@(sha256:[a-f0-9]{64})$`)
	tagName   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// release is one service's part of a deploy.
type release struct {
	svc    *Service
	ref    string // as requested: repo:tag or repo@sha256:...
	tag    string // the tag, if ref was given as one
	digest string // repo@sha256:..., once resolved; what actually runs
}

// parseRef splits a requested reference into repository and either a tag
// or a full digest reference.
func parseRef(ref string) (repo, tag, digest string, err error) {
	if m := digestRef.FindStringSubmatch(ref); m != nil {
		return m[1], "", ref, nil
	}
	i := strings.LastIndex(ref, ":")
	// A colon followed by a slash is a registry port, not a tag.
	if i <= 0 || strings.Contains(ref[i+1:], "/") {
		return "", "", "", fmt.Errorf("%q is neither repository:tag nor repository@sha256:<digest>", ref)
	}
	repo, tag = ref[:i], ref[i+1:]
	if !tagName.MatchString(tag) {
		return "", "", "", fmt.Errorf("%q has an invalid tag", ref)
	}
	if tag == "latest" {
		// The agent records a digest either way, but "latest" says nothing
		// about which build was meant, and is the tag most likely to have
		// moved again between the push and this pull.
		return "", "", "", fmt.Errorf("%q: deploy a per-commit tag or a digest, not latest", ref)
	}
	return repo, tag, "", nil
}

// resolve maps each ref the build host sent to the service that runs its
// repository. A repository the project doesn't track is an error, not a
// skip: it means .cicd.yaml and this config disagree about image names,
// and that mismatch is otherwise silent in both directions (green build,
// green deploy, production unchanged).
func (p *Project) resolve(refs []string) ([]release, error) {
	if len(refs) == 0 {
		return nil, errors.New("no images to deploy")
	}
	seen := map[string]bool{}
	var out []release
	for _, ref := range refs {
		repo, tag, digest, err := parseRef(ref)
		if err != nil {
			return nil, err
		}
		svc := p.serviceFor(repo)
		if svc == nil {
			return nil, fmt.Errorf("project %q has no service for repository %s (check services: in the agent config against images: in .cicd.yaml)", p.Name, repo)
		}
		if seen[svc.Name] {
			return nil, fmt.Errorf("service %q was given two images", svc.Name)
		}
		seen[svc.Name] = true
		out = append(out, release{svc: svc, ref: ref, tag: tag, digest: digest})
	}
	return out, nil
}

// Deploy rolls out refs (repo:tag, normally the per-commit tag j-cicd just
// pushed, or repo@sha256:...) for the named project: pull them, pin each
// to the digest it resolved to, recreate only the services whose digest
// changed, wait for them to report healthy, and roll back to the last
// healthy release if they don't. Any outcome other than "new release is
// healthy" is an error, so the build host's run fails too.
//
// compose, if not nil, is the compose file that goes with this release
// (sent by the build host from the commit it built). It is stored, used
// for this and later deploys, and restored by a rollback. Nil keeps
// whichever compose file is in use.
func (a *Agent) Deploy(ctx context.Context, projectName string, refs []string, compose []byte) error {
	proj, err := a.cfg.Project(projectName)
	if err != nil {
		return err
	}
	if proj.IsFiles() {
		return fmt.Errorf("project %q installs files (files: in the agent config), so it is deployed with artifacts, not images", proj.Name)
	}
	releases, err := proj.resolve(refs)
	if err != nil {
		return err
	}
	return a.locked(ctx, proj, func(st *State) error {
		target := st.composeCurrent()
		if compose != nil {
			if target, err = storeCompose(a.cfg.StateDir, proj.Name, compose); err != nil {
				return fmt.Errorf("storing the compose file: %w", err)
			}
		}
		return a.deploy(ctx, proj, st, releases, target)
	})
}

// Rollback reverts the most recent successful deploy of the project: each
// service it changed goes back to the release it replaced, through the
// same pull, pre_deploy, start and health check as any deploy (and the
// same automatic rollback if that fails). Services the last deploy didn't
// touch stay as they are. Running it again undoes it, since the release
// rolled back from becomes the previous one.
//
// Only images are rolled back. Whatever the replaced release did to its
// database stays done.
func (a *Agent) Rollback(ctx context.Context, projectName string) error {
	proj, err := a.cfg.Project(projectName)
	if err != nil {
		return err
	}
	if proj.IsFiles() {
		return a.rollbackFiles(ctx, proj)
	}
	return a.locked(ctx, proj, func(st *State) error {
		releases, err := lastDeployed(proj, st)
		if err != nil {
			return err
		}
		// The compose file goes back too if the last deploy changed it.
		compose := st.composeCurrent()
		if c := st.Compose; c != nil && len(releases) > 0 && c.CurrentSince.Equal(st.service(releases[0].svc).since()) {
			compose = c.Previous
		}
		a.log.Printf("rolling back %q's last deploy to %s", proj.Name, describe(releases))
		a.log.Printf("note: only images and the compose file are rolled back — database changes made by the release being replaced are not undone")
		return a.deploy(ctx, proj, st, releases, compose)
	})
}

// lastDeployed returns, for each service changed by the most recent
// successful deploy, its previous release.
func lastDeployed(proj *Project, st *State) ([]release, error) {
	var latest time.Time
	for i := range proj.Services {
		if ss := st.Services[proj.Services[i].Name]; ss != nil && ss.Current != "" && ss.since().After(latest) {
			latest = ss.since()
		}
	}
	if latest.IsZero() {
		return nil, fmt.Errorf("%q has never been deployed: nothing to roll back", proj.Name)
	}
	var out []release
	var stranded []string
	for i := range proj.Services {
		svc := &proj.Services[i]
		ss := st.Services[svc.Name]
		if ss == nil || ss.Current == "" || !ss.since().Equal(latest) {
			continue
		}
		if ss.Previous == "" {
			stranded = append(stranded, svc.Name)
			continue
		}
		out = append(out, release{svc: svc, ref: ss.Previous, tag: ss.PreviousTag, digest: ss.Previous})
	}
	if len(stranded) > 0 {
		return nil, fmt.Errorf("no previous release of %v recorded (it was the first deploy): nothing to roll back to", stranded)
	}
	return out, nil
}

// locked runs fn with the project's deploy lock held and its state loaded,
// so two deploys (or a deploy and a rollback) can't interleave.
func (a *Agent) locked(ctx context.Context, proj *Project, fn func(*State) error) error {
	if err := os.MkdirAll(a.cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	unlock, err := lockFile(ctx, filepath.Join(a.cfg.StateDir, proj.Name+".lock"), func() {
		a.log.Printf("another deploy of %q is in progress, waiting for it to finish", proj.Name)
	})
	if err != nil {
		return fmt.Errorf("taking deploy lock: %w", err)
	}
	defer unlock()

	st, err := LoadState(a.cfg.StateDir, proj.Name)
	if err != nil {
		return err
	}
	return fn(st)
}

// deploy is Deploy's body, with the lock held and state loaded. compose
// is the compose file to deploy with, by hash ("" for the configured one).
func (a *Agent) deploy(ctx context.Context, proj *Project, st *State, releases []release, compose string) error {
	dctx, cancel := context.WithTimeout(ctx, proj.timeout())
	defer cancel()

	current := st.composeCurrent()
	composeChanged := compose != current
	newProj, oldProj := a.withCompose(proj, compose), a.withCompose(proj, current)

	// Pull and resolve first, separately: if the registry is unreachable
	// or the tag doesn't exist, nothing has been touched yet.
	if err := a.pull(dctx, newProj, st, releases); err != nil {
		a.recordUntouched(proj, st, releases, resultUntouched)
		return fmt.Errorf("%w (nothing was changed)", err)
	}

	var changed []release
	for _, r := range releases {
		ss := st.service(r.svc)
		if ss.Current == r.digest && !composeChanged {
			a.log.Printf("[%s] already running %s", r.svc.Name, label(r))
			continue
		}
		if ss.Current == "" {
			a.log.Printf("[%s] warning: no previous release recorded, so this deploy cannot be rolled back", r.svc.Name)
		}
		changed = append(changed, r)
	}
	if composeChanged {
		// A new compose file can change any service's config (environment,
		// ports, healthcheck), so every deployed service is brought up
		// with it; compose itself recreates only those whose config
		// actually differs. Services never deployed are left alone.
		a.log.Printf("compose file changed (%s -> %s): applying it to every deployed service", shortHash(current), shortHash(compose))
		for i := range proj.Services {
			svc := &proj.Services[i]
			ss := st.Services[svc.Name]
			if ss == nil || ss.Current == "" || slices.ContainsFunc(changed, func(r release) bool { return r.svc == svc }) {
				continue
			}
			changed = append(changed, release{svc: svc, ref: ss.Current, tag: ss.CurrentTag, digest: ss.Current})
		}
	}
	if len(changed) == 0 {
		a.log.Printf("nothing to deploy for %q", proj.Name)
		return nil
	}

	// After the pull (a release that can't be pulled shouldn't cost a
	// backup) and before anything changes (so a failed backup blocks it).
	if err := a.preDeploy(dctx, proj, changed); err != nil {
		a.recordUntouched(proj, st, changed, resultPreDeployFailed)
		return fmt.Errorf("pre_deploy failed: %w (nothing was changed)", err)
	}
	// After the backup, so a migration that goes wrong has one to go back
	// to; before the swap, so the new release starts on a ready database.
	if err := a.preSwap(dctx, newProj, st, changed); err != nil {
		a.recordUntouched(proj, st, changed, resultPreSwapFailed)
		return fmt.Errorf("pre_swap failed: %w (nothing was changed)", err)
	}

	a.log.Printf("deploying %s", describe(changed))
	deployErr := a.start(dctx, newProj, st, changed)
	now := a.now().UTC()
	for _, r := range changed {
		ss := st.service(r.svc)
		ss.LastAttempt, ss.LastDeploy = r.ref, now
	}
	if deployErr == nil {
		for _, r := range changed {
			ss := st.service(r.svc)
			ss.Previous, ss.PreviousTag = ss.Current, ss.CurrentTag
			ss.Current, ss.CurrentTag, ss.CurrentSince, ss.LastResult = r.digest, r.tag, now, resultHealthy
		}
		if composeChanged {
			st.Compose = &ComposeState{Current: compose, Previous: current, CurrentSince: now}
		}
		if err := st.save(a.cfg.StateDir, proj.Name); err != nil {
			return fmt.Errorf("deploy succeeded but recording it failed: %w", err)
		}
		if err := st.pruneCompose(a.cfg.StateDir, proj.Name); err != nil {
			a.log.Printf("warning: removing old compose files: %v", err)
		}
		a.log.Printf("deployed %q: %s", proj.Name, describe(changed))
		return nil
	}

	a.log.Printf("deploy of %q failed: %v", proj.Name, deployErr)
	// Roll back even if the deploy was cancelled (Ctrl-C, or the build
	// host gave up): half-deployed is the worst state to leave behind.
	rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), proj.timeout())
	defer rbCancel()
	// Back on the compose file the running release was deployed with.
	rbErr := a.rollback(rbCtx, oldProj, st, changed)

	if err := st.save(a.cfg.StateDir, proj.Name); err != nil {
		a.log.Printf("warning: recording the failed deploy: %v", err)
	}
	if rbErr != nil {
		return fmt.Errorf("%w; ROLLBACK FAILED, needs attention: %v", deployErr, rbErr)
	}
	return fmt.Errorf("%w (rolled back to the previous release)", deployErr)
}

// recordUntouched records a deploy that stopped before changing anything.
func (a *Agent) recordUntouched(proj *Project, st *State, rs []release, result string) {
	now := a.now().UTC()
	for _, r := range rs {
		ss := st.service(r.svc)
		ss.LastAttempt, ss.LastDeploy, ss.LastResult = r.ref, now, result
	}
	if err := st.save(a.cfg.StateDir, proj.Name); err != nil {
		a.log.Printf("warning: recording the failed deploy: %v", err)
	}
}

// preDeploy runs the project's pre_deploy command, if it has one, with
// what's about to change in its environment.
func (a *Agent) preDeploy(ctx context.Context, proj *Project, changed []release) error {
	var parts []string
	for _, r := range changed {
		parts = append(parts, r.svc.Name+"="+shortLabel(r))
	}
	return a.runPreDeploy(ctx, proj, filepath.Dir(proj.ComposeFile), serviceNames(changed), parts)
}

// runPreDeploy runs pre_deploy (sh -c, from dir) with J_CICD_SERVICES set
// to names (services or files) and J_CICD_RELEASE to parts (name=release).
func (a *Agent) runPreDeploy(ctx context.Context, proj *Project, dir string, names, parts []string) error {
	if proj.PreDeploy == "" {
		return nil
	}
	a.log.Printf("pre_deploy: %s", proj.PreDeploy)
	return a.runner.Run(ctx, dir, "env",
		"J_CICD_PROJECT="+proj.Name,
		"J_CICD_SERVICES="+strings.Join(names, " "),
		"J_CICD_RELEASE="+strings.Join(parts, " "),
		"sh", "-c", proj.PreDeploy)
}

// pull fetches each release as requested, then fills in the digest it
// resolved to. From here on only the digest is used, so what runs (and
// what is recorded for rollback) can't change if the tag is moved later.
func (a *Agent) pull(ctx context.Context, proj *Project, st *State, rs []release) error {
	env := imageEnv(proj, st, rs, func(r release) string { return r.ref })
	if err := a.compose(ctx, proj, env, append([]string{"pull"}, serviceNames(rs)...)...); err != nil {
		return fmt.Errorf("pulling: %w", err)
	}
	for i := range rs {
		if rs[i].digest != "" {
			continue
		}
		digest, err := a.repoDigest(ctx, rs[i])
		if err != nil {
			return fmt.Errorf("resolving %s: %w", rs[i].ref, err)
		}
		rs[i].digest = digest
	}
	return nil
}

// repoDigest returns the repo@sha256:... docker recorded for r's tag when
// it pulled it.
func (a *Agent) repoDigest(ctx context.Context, r release) (string, error) {
	out, err := a.runner.Output(ctx, "", "docker", "image", "inspect",
		"--format", "{{range .RepoDigests}}{{println .}}{{end}}", r.ref)
	if err != nil {
		return "", err
	}
	prefix := r.svc.Repository + "@"
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); digestRef.MatchString(line) && strings.HasPrefix(line, prefix) {
			return line, nil
		}
	}
	return "", fmt.Errorf("docker recorded no digest for %s", r.ref)
}

// start recreates the given services on their resolved digests and waits
// for them to pass their health checks.
func (a *Agent) start(ctx context.Context, proj *Project, st *State, rs []release) error {
	env := imageEnv(proj, st, rs, func(r release) string { return r.digest })
	names := serviceNames(rs)
	// --no-deps: only the services that changed are recreated; stateful
	// dependencies like a database are never cycled by a deploy.
	if err := a.compose(ctx, proj, env, append([]string{"up", "-d", "--no-deps"}, names...)...); err != nil {
		return fmt.Errorf("starting: %w", err)
	}
	return a.waitHealthy(ctx, proj, env, names)
}

// rollback redeploys the last healthy release of each changed service and
// records the outcome in st. It deliberately goes exactly one step back
// and never retries the failed release.
func (a *Agent) rollback(ctx context.Context, proj *Project, st *State, changed []release) error {
	var back, stranded []release
	for _, r := range changed {
		ss := st.service(r.svc)
		if ss.Current == "" {
			ss.LastResult = resultNoRollback
			stranded = append(stranded, r)
			continue
		}
		back = append(back, release{svc: r.svc, ref: ss.Current, tag: ss.CurrentTag, digest: ss.Current})
	}

	var err error
	if len(back) > 0 {
		a.log.Printf("rolling back to %s", describe(back))
		// Pull again: the old image may have been pruned locally since.
		err = a.pull(ctx, proj, st, back)
		if err == nil {
			err = a.start(ctx, proj, st, back)
		}
		result := resultRolledBack
		if err != nil {
			result = resultRollbackFailed
		}
		for _, r := range back {
			st.service(r.svc).LastResult = result
		}
	}
	if len(stranded) > 0 {
		err = errors.Join(err, fmt.Errorf("no previous release of %v to roll back to; the failed release was left in place", serviceNames(stranded)))
	}
	return err
}

// imageEnv sets every tracked service's image variable to its recorded
// release (by digest), overridden by pick(r) for the releases given.
// Pinning the untouched services too keeps compose from seeing them as
// changed and from falling back to whatever default tag the compose file
// names.
func imageEnv(proj *Project, st *State, rs []release, pick func(release) string) []string {
	refs := map[string]string{}
	for _, svc := range proj.Services {
		if ss := st.Services[svc.Name]; ss != nil && ss.Current != "" {
			refs[svc.ImageVar] = ss.Current
		}
	}
	for _, r := range rs {
		refs[r.svc.ImageVar] = pick(r)
	}
	var env []string
	for _, svc := range proj.Services {
		if ref, ok := refs[svc.ImageVar]; ok {
			env = append(env, svc.ImageVar+"="+ref)
		}
	}
	return env
}

// compose runs `docker compose` for proj with env set, from the compose
// file's directory so relative paths in it resolve as they do by hand.
func (a *Agent) compose(ctx context.Context, proj *Project, env []string, args ...string) error {
	name, argv := composeArgv(proj, env, args)
	a.log.Printf("%s %s", name, strings.Join(argv, " "))
	return a.runner.Run(ctx, filepath.Dir(proj.ComposeFile), name, argv...)
}

func (a *Agent) composeOutput(ctx context.Context, proj *Project, env []string, args ...string) ([]byte, error) {
	name, argv := composeArgv(proj, env, args)
	return a.runner.Output(ctx, filepath.Dir(proj.ComposeFile), name, argv...)
}

// composeArgv goes through env(1) so the variables reach compose without
// CommandRunner needing an environment parameter.
func composeArgv(proj *Project, env []string, args []string) (string, []string) {
	argv := append([]string{}, env...)
	argv = append(argv, "docker", "compose")
	if proj.inUse != "" {
		// A release's compose file lives in the state dir; keep the
		// configured file's directory as the project directory so the
		// compose project (and its containers) and relative paths in the
		// file are the same as ever.
		argv = append(argv, "--project-directory", filepath.Dir(proj.ComposeFile), "-f", proj.inUse)
	} else {
		argv = append(argv, "-f", proj.ComposeFile)
	}
	if proj.EnvFile != "" {
		argv = append(argv, "--env-file", proj.EnvFile)
	}
	return "env", append(argv, args...)
}

// waitHealthy waits until every container of every named service passes
// its compose healthcheck, failing fast on one that reports unhealthy or
// stops, and failing on timeout. The compose file's healthchecks are the
// signal; the agent doesn't probe anything itself.
func (a *Agent) waitHealthy(ctx context.Context, proj *Project, env []string, services []string) error {
	timeout := proj.healthTimeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pending := append([]string{}, services...)
	stopped := func() error {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%s not healthy after %s", strings.Join(pending, ", "), timeout)
		}
		return ctx.Err()
	}
	warned := map[string]bool{}
	for {
		var still []string
		for _, svc := range pending {
			ready, noHealthcheck, err := a.serviceReady(ctx, proj, env, svc)
			if err != nil {
				if ctx.Err() != nil {
					return stopped()
				}
				return fmt.Errorf("service %s: %w", svc, err)
			}
			if noHealthcheck && !warned[svc] {
				a.log.Printf("[%s] warning: no healthcheck defined, so \"running\" is all that can be checked", svc)
				warned[svc] = true
			}
			if !ready {
				still = append(still, svc)
			}
		}
		if len(still) == 0 {
			a.log.Printf("healthy: %s", strings.Join(services, ", "))
			return nil
		}
		pending = still
		select {
		case <-ctx.Done():
			return stopped()
		case <-time.After(a.pollInterval):
		}
	}
}

// serviceReady reports whether all of svc's containers are up and healthy.
// An error means the service has definitively failed.
func (a *Agent) serviceReady(ctx context.Context, proj *Project, env []string, svc string) (ready, noHealthcheck bool, err error) {
	// -a: an exited container must be seen (and fail the deploy), not
	// mistaken for one that hasn't been created yet.
	out, err := a.composeOutput(ctx, proj, env, "ps", "-a", "-q", svc)
	if err != nil {
		return false, false, err
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return false, false, nil
	}
	ready = true
	for _, id := range ids {
		out, err := a.runner.Output(ctx, "", "docker", "inspect", "--format",
			"{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}", id)
		if err != nil {
			return false, false, err
		}
		fields := strings.Fields(string(out))
		if len(fields) != 2 {
			return false, false, fmt.Errorf("unexpected docker inspect output %q", out)
		}
		status, health := fields[0], fields[1]
		switch {
		case status == "exited" || status == "dead" || status == "restarting":
			return false, false, fmt.Errorf("container %s is %s", shortID(id), status)
		case health == "unhealthy":
			return false, false, fmt.Errorf("container %s is unhealthy", shortID(id))
		case health == "healthy":
		case health == "none" && status == "running":
			noHealthcheck = true
		default:
			ready = false
		}
	}
	return ready, noHealthcheck, nil
}

func serviceNames(rs []release) []string {
	names := make([]string, len(rs))
	for i, r := range rs {
		names[i] = r.svc.Name
	}
	return names
}

func describe(rs []release) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.svc.Name + "=" + label(r)
	}
	return strings.Join(parts, ", ")
}

// label names a release for humans: its tag (the commit) when there is
// one, plus the digest it resolved to.
func label(r release) string {
	if r.tag == "" {
		return r.digest
	}
	if r.digest == "" {
		return r.ref
	}
	return r.tag + " (" + ShortDigest(r.digest) + ")"
}

// ShortDigest abbreviates repo@sha256:<64 hex> to sha256:<12 hex>.
func ShortDigest(ref string) string {
	_, d, ok := strings.Cut(ref, "@")
	if !ok {
		d = ref
	}
	if len(d) > len("sha256:")+12 {
		d = d[:len("sha256:")+12]
	}
	return d
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// shortLabel is label without the digest: the tag, else the short digest.
func shortLabel(r release) string {
	if r.tag != "" {
		return r.tag
	}
	return ShortDigest(r.digest)
}

// withCompose returns proj set up to run the compose file with the given
// hash ("" for the configured file).
func (a *Agent) withCompose(proj *Project, hash string) *Project {
	if hash == "" {
		return proj
	}
	p := *proj
	p.inUse = composePath(a.cfg.StateDir, proj.Name, hash)
	return &p
}

// shortHash names a compose file by hash for humans.
func shortHash(hash string) string {
	if hash == "" {
		return "configured file"
	}
	return "sha256:" + shortID(hash)
}
