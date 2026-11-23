# j-cicd

A self-hosted CI/CD pipeline tool, written in Go, meant to run on any machine
(VPS, homelab box, laptop, etc). It builds a given project, runs its tests,
and pushes the resulting artifact (likely a container image) to a registry
that a target VPS can then pull from to deploy.

## Goals

- **Portable runner**: single Go binary, no heavyweight CI server required.
- **Pipeline**: clone/checkout -> build -> test -> package -> push to image
  registry.
- **Deploy-side helper**: a small companion program (`j-cicd-agent`) that
  runs on the VPS and, when the build host tells it to over SSH, pulls the
  new images and redeploys them, rolling back if they come up unhealthy.
- **Stretch goal**: on pipeline failure, automatically spin up an agent to
  investigate the failure and report findings/next steps back to the user
  (e.g. via Slack, email, or similar).

## Status

Basic CI/CD runner works end to end: `j-cicd run` loads a project's
`.cicd.yaml`, runs its shell steps in order, and builds (and optionally
pushes) a Docker image per entry in `images:` via the `docker` CLI. A
project can have multiple `images:` entries (e.g. separate frontend/backend
services, each with their own Dockerfile) — `j-cicd init` auto-detects these
by scanning for Dockerfiles rather than assuming a single one at the repo
root. Every command runs under a cancellable context with a timeout, and
images are tagged with the commit they were built from as well as `latest`.

j-cicd is also a central manager for every project it builds: projects are
registered once (`j-cicd add`) and then run by name from anywhere, every
run is recorded with its outcome and full output, and bare `j-cicd` opens
an interactive dashboard to pick a project, start or cancel a run, and
browse history and logs (live while a run is going).

Deploy works too: if `.cicd.yaml` has a `deploy:` section, the run's last
stage runs `ssh <host> j-cicd-agent deploy <project> <repo:<commit>>...`.
The agent on the VPS pulls that per-commit tag, pins it to the digest it
resolved to, recreates only the compose services whose digest changed,
waits for their healthchecks, and rolls back to the last healthy release
on failure; its exit status passes or fails the run. An optional
`pre_deploy` command in the agent's config (e.g. a database backup) runs
before any container changes, and an optional `pre_swap` command (e.g.
database migrations) then runs once inside one service's *new* image via
`docker compose run`; a failure or timeout of either aborts the deploy with
nothing changed. Projects that ship binaries instead of images (a CLI,
a systemd service) list them under `artifacts:`; the deploy sends the files
by sha256 and the agent installs them atomically, restarts their unit,
waits for it to stay up, runs a check, and rolls back the same way — on
the VPS over ssh, or on this machine with `deploy.local` (j-cicd installs
itself into `~/.local/bin` this way). `j-cicd rollback <name>` (or `R` in the
dashboard) reverts the last deploy by hand — images or files, never the
database — and is recorded in history as a `rollback` run. See [AGENT.md](AGENT.md) for the
design and VPS setup. Not yet implemented: the stretch-goal
failure-investigation agent.

j-cicd is project-agnostic by design: it takes a project directory and that
project's own `.cicd.yaml` as input. `simpletrack` is just the first project
pointed at it — nothing in the tool assumes a specific project, and running
it against a second or third project just means writing another config.

## Config format (`.cicd.yaml`, lives in the target project's repo)

```yaml
name: my-project       # required
branch: main           # optional release branch: build its latest commit, not the working tree
remote: origin         # optional, with branch: fetch it and build the remote's copy, not the local branch
timeout: 30m            # optional, default time limit per step/docker command
steps:                 # optional list of shell commands, run in order
  - name: build
    run: go build ./...
  - name: test
    run: go test ./...
    dir: subdir         # optional, relative to the project dir
    timeout: 10m        # optional, overrides the top-level timeout
images:                 # optional list — omit if there's no docker stage
  - name: backend        # optional, just a label for logs
    repository: ghcr.io/owner/my-project-backend
    dockerfile: backend/Dockerfile   # default: Dockerfile
    context: backend                 # default: .
    tags: [latest]                   # default: [latest, <git revision>]
  - name: frontend
    repository: ghcr.io/owner/my-project-frontend
    dockerfile: frontend/Dockerfile
    context: frontend
```

Each entry in `images:` is built and (optionally) pushed independently, so
multi-service projects just list one entry per service/Dockerfile.

A project that ships files rather than images lists them as `artifacts:`
(what its steps leave behind):

```yaml
artifacts:
  - name: mini-bot          # what the agent's config calls it (files: name)
    path: dist/mini-bot     # relative to the project dir
```

After the steps each must exist (a missing one fails the run by name); its
sha256 identifies the release the way a digest does an image, and is
recorded in the run (`artifacts` in `--json`). A deploy of artifacts
streams them to the agent as a tar on the ssh connection's stdin,
`j-cicd-agent deploy <project> name@sha256:<hex>... --artifacts-stdin
--revision <rev>`; the agent refuses any file that doesn't hash to its ref.
A deploy sends images or artifacts, never both: with both in the config,
`deploy.images` or `deploy.artifacts` says which. Build-only runs
(`--no-push`, `b`) still build and hash them, but don't deploy.

An optional `deploy:` section hands the pushed images to the VPS:

```yaml
deploy:
  host: deploy@vps.example.com   # ssh destination or ~/.ssh/config alias
  # local: true                  # instead of host: run the agent on this machine (sh -c, no ssh)
  command: j-cicd-agent          # optional: how to invoke the agent remotely
  project: my-project            # optional, default name: — the project in the agent's config
  images: [backend]              # optional, default all images: entries (by name)
  # artifacts: [mini-bot]        # optional, the same for artifacts: entries
  timeout: 10m                   # optional, default the top-level timeout
  compose_file: deploy/compose.yaml   # optional: sent with the deploy
  verify:                        # optional: checks of the live site after the deploy
    - url: https://app.example.com/version.json
      expect_body_contains: "{{revision}}"   # default: any body
      expect_status: 200                      # default 200
      timeout: 2m                             # keep retrying this long (default 2m; 0 = once)
```

**Post-deploy verify.** The agent only knows the containers are healthy;
`verify:` asks the public site (from the build host, the way users reach it)
whether it's serving this release. After a successful deploy each check
GETs its URL (with a cache-busting query parameter and `Cache-Control:
no-cache`), retrying every 3s until the status and body match or its
timeout passes. `{{revision}}` in the url or body is the deployed
revision. A failure fails the run (recorded with `deployed: true`, since
the release *is* running) and the error names `j-cicd rollback <name>`; it
never rolls back by itself. Each passed check is reported with how many attempts and seconds it took
(the log, the concise output, and `verified` in the run's JSON). It doesn't run without a deploy (`--no-push`,
`--no-deploy`). The app must expose its commit somewhere for this to be
useful: every step and docker command gets `J_CICD_REVISION` in its
environment (the same revision the images are tagged with), so a step like
`echo "{\"commit\":\"$J_CICD_REVISION\"}" > public/version.json` before the
image build is enough. The path should be served with `Cache-Control:
no-store` so a cache can't answer for it.

Deploy sends each image's per-commit tag (`repo:<revision>`), never
`latest`; the agent turns it into a digest (`repo@sha256:...`, the
registry's content hash, which can't be moved the way a tag can) and runs
and records that. It runs only when the images were pushed in this run,
and is refused **before anything builds** if there's no per-commit tag
(not a git repo, or explicit `tags:` without the revision) or if the tree
is dirty — `--deploy-dirty` overrides the latter. Which compose service and image variable each
repository maps to lives in the agent's own config on the VPS, next to the
compose file — not here.

The config is validated strictly, because it decides what gets published to
a registry:

- **Unknown keys are an error.** A typo like `dockerFile:` or `tag:` would
  otherwise be dropped silently and the build would quietly use defaults.
- **A step must have a `run:`.** An empty one would run as `sh -c ""` and
  exit 0, which makes "no test ran" look like "tests passed".
- **Two images may not push the same `repository:tag`.** Otherwise both get
  built, both get pushed, the registry keeps only the last, and a VPS
  pulling that tag gets whichever happened to sort last.

**Tags.** When an image declares no `tags:`, it gets `latest` plus the short
git revision of the project dir (e.g. `[latest, 7fc0b4b12345]`), so every
build leaves behind an immutable handle to roll back to or to map a running
container to a commit. A tree with uncommitted changes is tagged
`<rev>-dirty`, since j-cicd builds what's on disk rather than what's
committed. Declaring `tags:` explicitly overrides all of this.

**Release remote.** With `remote:` (needs `branch:`) a run first does
`git fetch <remote> <branch>` and builds `<remote>/<branch>`, so a PR merged
on the remote ships without anyone pulling; the local branch and checkout
are never touched. A failed fetch (2 minute limit, no credential prompts)
fails the run rather than building something stale. Local commits missing
from the remote are a warning, being behind isn't. `--plan` fetches too, so
the plan names the commit a run would build. `run --commit <rev>` pins a
release run to one commit of the branch's history (what `--plan` printed),
so a run can't build something other than what was reviewed; a commit
outside the history is refused.

**Release branch.** With `branch:` set, a run never builds the project
directory as it is on disk. It resolves the local branch to its latest
commit, checks that commit out as a detached `git worktree` at
`<state root>/snapshots/<project>`, runs everything (steps, builds, push,
deploy) there with the config *committed on that branch*, and removes the
worktree afterwards. So the checked-out branch and uncommitted changes in
the project dir never block a release and never leak into one; the
revision is always the branch's clean commit, so deploys work without
`--deploy-dirty`. Build-only runs (`--no-push`, `b` in the dashboard)
build the branch too. A snapshot holds only what's committed — no
`node_modules`, `.env` or build output — so the steps must install what
they need. It's the local branch, so committing is enough; pushing to a
remote isn't required. A missing branch, a project dir outside git, or a
branch with no committed `.cicd.yaml` fails the run with a message saying
which. Without `branch:`, the directory is built as before.

**Docker check.** A run with any `images:` first runs `docker version`,
before any step, so an unusable Docker fails the run in a second with a
one-line reason and fix as the run's error (shown in `history` and the
dashboard), not as a failed `docker build` after the steps. On failure it
probes the local socket to say which problem it is: CLI not installed,
daemon not running, user not in the socket's group, or in the group but
the session predates it (needs a reboot). A daemon that's simply stopped
is started with `systemctl start --no-ask-password docker` (Linux only);
when that needs a password, the error says to run
`sudo systemctl enable --now docker`. Remote or non-default docker
contexts get the generic message.

**Timeouts.** Each step and each docker command runs under a deadline:
`timeout:` on the step, else `timeout:` at the top level, else the runner's
built-in default (`pipeline.DefaultTimeout`, 60m). `timeout: 0` means no
limit. Steps run in their own process group, so a timeout (or Ctrl-C) kills
the whole step, not just the `sh -c` wrapper around it.

Registry auth is whatever `docker login` already has configured on the
host — j-cicd just shells out to `docker build`/`docker push`, it doesn't
manage credentials itself. ghcr.io (GitHub Container Registry) works fine
this way; see the project owner's own notes for the one-time
`docker login ghcr.io` setup.

`j-cicd init` scans the project directory for Dockerfiles (skipping
`node_modules`/`vendor`/`dist`/`build`/dotfiles, up to 4 levels deep, and
ignoring non-buildable `Dockerfile.*` files like `Dockerfile.md` or editor
backups) and generates one `images:` entry per Dockerfile found. The service
label comes from the Dockerfile's whole relative path plus any
`Dockerfile.<variant>` suffix (`services/api/Dockerfile` ->
`services-api`, `Dockerfile.dev` -> `dev`), so no two entries can collapse
onto the same generated `repository:`. If the project's git `origin` remote
points at GitHub, the generated `repository:` defaults to
`ghcr.io/<owner>/<project>-<service>`; otherwise it falls back to a
`registry.example.com/...` placeholder that needs editing by hand. Generated
configs deliberately omit `tags:` so each run applies the tag defaults above.

## Building

Plain Go, no other build tooling (Fedora is the primary dev machine; Go and
Docker come from the system packages):

- `go build -o bin/ ./cmd/...` — build `bin/j-cicd` and `bin/j-cicd-agent`.
- `GOBIN=~/.local/bin go install ./cmd/...` — install both onto `PATH`.
  Plain `go install` puts them in `~/go/bin`, which isn't on `PATH` by
  default on Fedora; `~/.local/bin` is. Re-run after pulling changes.
- `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/j-cicd-agent-linux-amd64 ./cmd/j-cicd-agent`
  — a static agent binary to copy to the VPS.
- `gofmt -l . && go vet ./... && go test ./...` — the checks.

`version` prints `-X main.version=...` if set via `-ldflags`, otherwise the
git revision Go stamps into the binary (`-dirty` for uncommitted changes),
otherwise `dev`. Go only stamps that from a regular checkout — builds from a
`git worktree` report `dev`.

## CLI

- `j-cicd` — at a terminal, opens the dashboard (same as `j-cicd ui`);
  otherwise prints usage.
- `j-cicd add [project-dir] [--name n] [--config path]` — register a
  project. The config must load; the name defaults to the config's `name:`.
- `j-cicd remove <name>` — unregister (history and files are kept).
- `j-cicd list [--json]` — projects with their latest result.
- `j-cicd run [name|project-dir] [--config path] [--no-push] [--no-deploy]
  [--deploy-dirty] [--commit rev] [--plan] [--json] [-v|--verbose] [--quiet]` —
  run a pipeline. A bare word that names a registered project runs it;
  anything path-like is a directory as before. A registered directory is
  recorded under its registered name. See **Output and agents** below for
  `--plan`, `--json`, `-v` and `--quiet`.
- `j-cicd deploy [name|project-dir] [run's flags]` — `run --plan` and
  `run --commit <planned rev>` in one call: resolves the plan once, prints
  its key lines to stderr, then runs pinned to that commit. Refuses `--plan`,
  `--commit`, `--no-push` and `--no-deploy`; a project without `deploy:` is a
  usage error.
- `j-cicd check [name|project-dir] [--ref commit-ish] [--config path] [--json]
  [--brief] [-v] [--quiet]` — run only the `steps:` (no docker check, images,
  artifacts or deploy), so "will this pass?" reads the same config as the
  pipeline. Without `--ref` it checks the directory as on disk, under the
  project lock; with it, a clean throwaway worktree of that commit with the
  config committed there (no lock), so a feature branch can be checked while
  `branch:` names the release branch. Recorded as a run of kind `check`
  (`manager.Check`, `pipeline.Options.StepsOnly`).
- `--brief` (`run`, `deploy`, `rollback`, `check`) — stdout is one line on
  success (`ok <project> <rev> deployed verified 43s`) and, on failure, only a
  failure object (`status`, `id`, `failure`, `exit_code`, `next`, no stages).
  Errors before a run is recorded are the usual `--json` error object.
- `j-cicd history [name] [-n count | --limit count | --last] [--json]` —
  recorded runs, newest first.
- `j-cicd logs <run-id|name> [-f] [--tail N] [--stage s] [--grep re [-C N]]
  [--plain|--raw]` — a run's output (a name means its latest run); `-f`
  follows it until it finishes (raw), `--tail N` prints only the last N
  lines. The log has a `== stage <text> ==` marker where each stage starts;
  `--stage` prints one stage by name or by kind and name (`test`,
  `step test`, `build app`, `verify https://…`; a name must match exactly),
  `--grep` filters lines with `-C` context. Off a terminal (or with
  `--plain`) colour codes and progress-bar redraws are stripped; `--raw`
  prints the recorded bytes.
- `j-cicd cancel <run-id|name>` — stop a run in progress.
- `j-cicd rollback [name|project-dir] [--config path] [--json] [-v] [--quiet]` — revert
  the last deploy on the VPS via `j-cicd-agent rollback` over ssh, using
  the config's `deploy:` section. Recorded as a run of kind `rollback`.
- `j-cicd init [project-dir] [--name n] [--force]` — scaffold a starter
  `.cicd.yaml`.
- `j-cicd guide [--short]` — print the operating guide (`--short`: only the
  "Short version" section at its top) for AI agents using j-cicd
  (`internal/guide/agent.md`, embedded in the binary so it always matches the
  installed version). Project CLAUDE.md files should point agents here rather
  than restate how j-cicd works.
- `j-cicd version`
- `j-cicd-agent deploy <project> <repo:tag|repo@sha256:...>... [--config path]` — on
  the VPS: deploy, health-check, roll back on failure. Normally run by
  `j-cicd run` over ssh rather than by hand.
- `j-cicd-agent deploy <project> <name@sha256:...>... [--artifacts-stdin]
  [--revision rev]` — the same for a files project: receive the files
  (a tar on stdin), install, restart, check, roll back on failure. Without
  `--artifacts-stdin` it deploys releases already in the agent's store.
- `j-cicd-agent rollback <project> [--config path]` — put the services the
  last successful deploy changed back on their previous release (pull,
  `pre_deploy`, up, health check). Running it again undoes it.
- `j-cicd-agent status [project] [--config path]` — what's deployed, what it
  rolls back to, and how the last deploy went.
- `j-cicd-agent version`

The agent's config is `--config`, else `$J_CICD_AGENT_CONFIG`, else
`/etc/j-cicd-agent.yaml`.

Flags work on either side of positional arguments. Unknown flags and stray
positional arguments are errors rather than being ignored. Exit codes: 0 ok,
1 pipeline failed, 2 usage error, 3 config could not be loaded — the same
for both binaries.

## Output and agents

j-cicd is used by people at a terminal and by scripts and AI agents, so
output depends on who is reading:

- **At a terminal** (stderr is a TTY), `run` streams everything as it
  happens, as before.
- **Anywhere else** (a pipe, a script, an agent) it prints a concise
  account to stderr: `run <id> started`, one `• step <name>` /
  `• building image <x>` / `• pushing image <x>` / `• deploying to <host>`
  line per stage, any warnings, and, if the run fails, the last 40 lines of
  the *failing stage's* output. The full output is always recorded;
  `j-cicd logs <id>` (or `--tail N`) gets it.
- `-v`/`--verbose` forces the full stream; `--quiet` prints only the final
  status line. `--json` implies the concise form unless `-v` is given.
- A final stderr line gives the run id, commit, status, duration, the
  registry digest of each pushed image and where it was deployed. A failed
  run adds `failure: kind=step stage=test exit=3` (kind, stage, exit status,
  `timed_out=true` — whichever apply). Off a terminal stderr carries no
  timestamps (the run log keeps them).
- **Failures are structured.** Every run records `stages` (name, kind,
  status ok|failed|running — saved as each stage starts, so `running`
  shows only for a run still going or killed — seconds, `exit_code` of a failing command,
  `timed_out`) and, if it didn't succeed, a `failure`: `kind` (a stage kind —
  step, build, push, artifacts, deploy, verify, docker-check, preflight,
  rollback — or config, cancelled, run), `stage`, `exit_code`, `timed_out`,
  `message` (the same text as `error`) and `tail`, the failing stage's last
  40 lines cleaned of ANSI codes and progress-bar redraws (for a verify
  failure, the check's attempts, which no command printed). A stage with no
  name of its own (docker-check, preflight, artifacts) uses its kind, and
  its log marker is just `== stage artifacts ==`; `next` always gives the
  unambiguous `--stage 'step test'` form. All of it is in
  `run.json`, so it survives the process and appears in `history --json`.
  Code: `pipeline.StageError`/`Stages()` (`internal/pipeline/stage.go`),
  `failureOf` in `internal/manager/manager.go`, cleaning in
  `internal/manager/ansi.go`.
- **`--json`** (`run`, `rollback`, `list`, `history`) prints machine-readable
  results on stdout — for `run` one object with `id`, `status`, `error`,
  `revision`, `images` (name, repository, tags, `digest`), `deployed`,
  `deploy_host`, `stages`, `failure`, `duration_seconds`, `exit_code` (the
  status j-cicd returns) and, if it didn't succeed, `next` (runnable
  commands: the failing stage's log, a rollback when something is already
  deployed) — even when the run failed. When no run was recorded (unknown
  project, bad flag, busy project, config that won't load) it is still one
  object, `{"status":"error","error_kind":"usage|config|not_found|busy|
  internal","error":...,"exit_code":N,"next":[...]}`; `--plan` errors too.
  Stdout carries only data; prose and progress always go to stderr.
- **Exit status**: 0 ok, 1 failed or cancelled, 2 usage, 3 config. It is
  j-cicd's own only if it isn't piped (`| tail` replaces it with tail's;
  use `set -o pipefail` or `--json`).
- **`run --plan`** works out what a run would do — the commit, steps,
  images with tags, deploy host and refs, warnings — without running a
  step, building, pushing, deploying or recording a run (`--json` too). It
  checks out a release branch into a throwaway worktree to read the config
  and compose file as committed. A run that would be refused (dirty tree,
  no per-commit tag) lists a PROBLEM and exits 1.
  `--plan --json` is one object: `project`, `branch`, `revision`, `steps`,
  `images`, `deploy` (host, refs, `verify` URLs), `skipped`, `warnings`,
  `problems`, `remote` (when `remote:` is set) and `upstream`/`ahead`/
  `behind` (with `remote:`, `ahead` counts unpushed local commits left out
  of the build and `behind` is omitted).
- **Local vs. origin**: without `remote:`, j-cicd builds the *local* branch (the
  release `branch:`, or the checked-out one), and says so. If it's ahead of,
  behind, or diverged from its upstream (as of the last fetch; j-cicd never
  touches the network for this, except the fetch `remote:` asks for) it warns, in the run, the plan and the
  concise output. The plan always states
  the relationship: in sync, ahead/behind, or no upstream to compare with.

## State and history

Everything lives under one root: `$J_CICD_HOME`, else
`$XDG_STATE_HOME/j-cicd`, else `~/.local/state/j-cicd`.

- `projects.json` — the registry (name, absolute dir, optional config).
- `runs/<id>/run.json` + `runs/<id>/output.log` — one dir per run. IDs are
  `YYYYMMDD-HHMMSS-<hex>` (UTC), so they sort by start time. The newest 50
  finished runs per project are kept; older ones are pruned.
- `snapshots/<project>/` — the release-branch checkout of a run in
  progress (see **Release branch**); removed when the run ends, and
  replaced if a killed run left one behind.
- `locks/<project>.lock` — `flock`ed for the whole run, so only one run per
  project executes at a time, across every j-cicd process. The kernel
  releases it however the process dies.
- `launcher.log` — stderr of background runs started from the dashboard.

A run records the PID of the process executing it. A `running` record whose
process is gone (SIGKILL, crash, reboot) is rewritten as `interrupted` the
next time it's read. Cancelling is a SIGTERM to that PID, which j-cicd
handles by killing the step's process group and recording `cancelled`.

The dashboard never runs pipelines in-process: starting a run launches a
detached `j-cicd run --quiet <name>` (own session), and the dashboard just
re-reads the store every ~700ms. So quitting it leaves builds running, and
CLI-started runs show up in it too. Push runs ask for confirmation; `b`
builds without pushing.

## Code structure

- `cmd/j-cicd/` — `package main`: the CLI and nothing else.
  - `main.go` stays minimal: it only calls `appStart()`.
  - `app.go` owns application startup, dispatches `os.Args` to a
    subcommand, and is the **only** place that calls `os.Exit` — subcommands
    return errors (optionally wrapped in `exitError` to pick an exit code)
    so deferred cleanup still runs and the future failure-investigation
    agent is reachable after a failure.
  - `commands.go` — CLI flag parsing per subcommand (`run`, `init`,
    `version`), and all user-facing reporting for them; `jsonout.go` is
    the `--json` encoding.
- `cmd/j-cicd-agent/` — `package main` for the VPS agent, same
  `main.go`/`app.go`/`commands.go` split and the same "only `app.go` exits"
  rule (so the deploy lock is always released).
- `internal/cli/` — argument parsing shared by both binaries (flags on
  either side of positionals) and the version fallback.
- `internal/config/` — `.cicd.yaml` schema, loading, defaulting, validation
  (`config.go`), and the `deploy:` section (`deploy.go`).
- `internal/pipeline/` — runs a project's steps in order, stops on first
  failure, then builds/pushes each image.
  - `pipeline.go` — the orchestration state machine.
  - `docker.go` — assembles `docker build`/`docker push` invocations.
  - `dockercheck.go` — the pre-run Docker check, diagnosis, and
    auto-start; `dockercheck_unix.go` probes the socket and groups.
  - `deploy.go` — the deploy preconditions (per-commit tag, clean tree)
    and the agent invocation (`ssh ... j-cicd-agent deploy`, or `sh -c`
    for a local deploy).
  - `artifacts.go` — checking and hashing `artifacts:` after the steps,
    and the tar stream a deploy sends them in.
  - `runner.go` — the `CommandRunner` seam plus the real `exec`-based
    implementation; `runner_unix.go` adds process-group cancellation.
- `internal/agent/` — the VPS side: agent config (`config.go`), the
  deploy/health-wait/rollback sequence (`agent.go`), the `pre_swap` hook
  (`preswap.go`), files projects — blob store, atomic install, drift
  adoption, rollback (`files.go`) — and their systemd restart and settle
  check (`systemd.go`), per-project JSON state
  (`state.go`), and the `flock` deploy lock (`lock_unix.go`).
- `internal/detect/` — project inspection: Dockerfile scan, GitHub remote
  and git revision detection (`detect.go`), and the `init` scaffolding built
  on top of it (`starter.go`).
- `internal/store/` — persistent state: project registry (`projects.go`),
  run records and pruning (`runs.go`), per-project locks (`lock.go`).
  Plain JSON written atomically (temp file + rename).
- `internal/manager/` — executes a project as a recorded run: lock, record
  start, send all output to the run log (optionally echoed), record the
  outcome. Also `Launch` (detached background run) and `Cancel`.
  Every pipeline run, CLI or dashboard, goes through `manager.Run`.
  `plan.go` is `run --plan`; `tail.go` keeps a failed stage's last lines
  for concise output.
  `snapshot.go` resolves a release `branch:` and manages its worktree
  checkout (plain `git` via `os/exec`, like `detect`: it's repository
  plumbing, not a pipeline command, and its tests use real repos).
- `internal/display/` — shared formatting of runs (status symbols, ages,
  durations) for the CLI tables and the dashboard.
- `internal/tui/` — the Bubble Tea dashboard (`tui.go`) and its log viewer
  (`logview.go`). It reads the store and launches runs through a callback;
  it never calls the pipeline directly.
- `cmd/j-cicd/manage.go` — the registry/history subcommands (`add`,
  `remove`, `list`, `history`, `logs`, `cancel`, `ui`).
- As more lands (failure-investigation agent, ...), give it its own
  file or package rather than growing an existing one indefinitely.

## Conventions

- Standard Go idioms: `gofmt`/`go vet` clean, camelCase identifiers,
  errors returned and handled explicitly (no panics for expected failures).
- Exported identifiers mean "part of this package's API". Keep helpers
  unexported; `package main` exports nothing.
- All process-level I/O (printing, exiting, signal handling) belongs at the
  command boundary in `cmd/j-cicd` / `cmd/j-cicd-agent`, so the logic underneath stays callable
  and testable. Progress output goes through `log` to stderr.
- Anything that shells out goes through `pipeline.CommandRunner`, and
  anything long-running takes a `context.Context` as its first parameter.
- Features land with tests. The pure helpers (config validation,
  `serviceName`, the Dockerfile scan over a `t.TempDir()`) are tested
  directly; the runner is tested with a fake `CommandRunner` that records
  the commands it was asked to run. `go test ./...` needs no docker daemon.
  Anything touching state uses a `store.Open(t.TempDir())`; `cmd/j-cicd`
  tests set `J_CICD_HOME` in `TestMain`, so tests never touch the real
  registry. The dashboard is tested by driving the model's key handling
  and checking `View()`.
- Prefer the standard library where practical; only reach for a dependency
  when it clearly earns its keep (e.g. a registry client, YAML parsing,
  Bubble Tea/Lip Gloss for the dashboard).
- No half-finished features behind TODOs — land things when they work end
  to end, even if narrow in scope.

## Keeping the agent guide current

`internal/guide/agent.md` is the single source of truth for how an agent
operates j-cicd. A user-visible change to a command, flag, exit code or
deploy behaviour updates it in the same commit; `cmd/j-cicd/guide_test.go`
fails if a subcommand in the usage text or a key flag isn't named in it (in
backticks). That checks names only, so a passing test doesn't mean the
guide's behaviour descriptions are current — review those by hand.

## Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/):
`<type>: <summary>` (optionally `<type>(<scope>): <summary>`), with the
summary in the imperative, lowercase after the colon, no trailing period and
about 72 characters at most. Types:

- `feat:` new behaviour users or the agent can see
- `fix:` a bug fix
- `refactor:` restructuring with no behaviour change
- `test:` tests only
- `docs:` documentation only (including this file)
- `chore:` tooling, dependencies, config, housekeeping
- `ci:` j-cicd's own `.cicd*.yaml` pipelines
- `perf:` performance only

Mark breaking changes (config schema, CLI flags, agent protocol) with `!`
(`feat!:`) and a `BREAKING CHANGE:` footer. The body says *why*, not what the
diff already shows. One logical change per commit, and work happens on a
branch (`feat/...`, `fix/...`) merged through a PR, not committed straight to
`main`. History before this section predates the convention.
