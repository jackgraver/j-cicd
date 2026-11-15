# j-cicd

A self-hosted CI/CD pipeline tool, written in Go, meant to run on any machine
(VPS, homelab box, laptop, etc). It builds a given project, runs its tests,
and pushes the resulting artifact (likely a container image) to a registry
that a target VPS can then pull from to deploy.

## Goals

- **Portable runner**: single Go binary, no heavyweight CI server required.
- **Pipeline**: clone/checkout -> build -> test -> package -> push to image
  registry.
- **Deploy-side helper**: a small companion program/agent that runs on the
  VPS, watches for new images, and handles pulling + redeploying.
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

Not yet implemented: the VPS-side deploy helper, and the stretch-goal
failure-investigation agent.

j-cicd is project-agnostic by design: it takes a project directory and that
project's own `.cicd.yaml` as input. `simpletrack` is just the first project
pointed at it — nothing in the tool assumes a specific project, and running
it against a second or third project just means writing another config.

## Config format (`.cicd.yaml`, lives in the target project's repo)

```yaml
name: my-project       # required
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

## Nix

A `flake.nix` packages j-cicd with `buildGoModule` (NixOS is the primary dev
machine). Useful commands:

- `nix build .#default` — build the binary into `./result/bin/j-cicd`.
- `nix profile install .#default` — install it into the user profile so
  `j-cicd` is on `PATH` globally (re-run after dependency/version changes).
- `nix develop` — drop into a shell with `go` and `docker` available.

The derivation builds `subPackages = [ "cmd/j-cicd" ]` and stamps the
version in with `ldflags = [ "-X main.version=${version}" ]`, so the flake
is the single source of truth for the version string and a plain `go build`
outside Nix reports `dev`.

Note that `src = ./.` under flakes only sees **git-tracked** files: a new
file has to be `git add`ed before `nix build` can compile it.

If `go.mod`/`go.sum` change (new or updated dependency), `vendorHash` in
`flake.nix` needs to be recomputed: set it to `pkgs.lib.fakeHash`, run
`nix build .#default`, and copy the "got:" hash from the mismatch error back
into `vendorHash`.

## CLI

- `j-cicd` — at a terminal, opens the dashboard (same as `j-cicd ui`);
  otherwise prints usage.
- `j-cicd add [project-dir] [--name n] [--config path]` — register a
  project. The config must load; the name defaults to the config's `name:`.
- `j-cicd remove <name>` — unregister (history and files are kept).
- `j-cicd list` — projects with their latest result.
- `j-cicd run [name|project-dir] [--config path] [--no-push] [--quiet]` —
  run a pipeline. A bare word that names a registered project runs it;
  anything path-like is a directory as before. A registered directory is
  recorded under its registered name. `--quiet` only records output.
- `j-cicd history [name] [-n count]` — recorded runs, newest first.
- `j-cicd logs <run-id|name> [-f]` — a run's output (a name means its
  latest run); `-f` follows it until it finishes.
- `j-cicd cancel <run-id|name>` — stop a run in progress.
- `j-cicd init [project-dir] [--name n] [--force]` — scaffold a starter
  `.cicd.yaml`.
- `j-cicd version`

Flags work on either side of positional arguments. Unknown flags and stray
positional arguments are errors rather than being ignored. Exit codes: 0 ok,
1 pipeline failed, 2 usage error, 3 config could not be loaded.

## State and history

Everything lives under one root: `$J_CICD_HOME`, else
`$XDG_STATE_HOME/j-cicd`, else `~/.local/state/j-cicd`.

- `projects.json` — the registry (name, absolute dir, optional config).
- `runs/<id>/run.json` + `runs/<id>/output.log` — one dir per run. IDs are
  `YYYYMMDD-HHMMSS-<hex>` (UTC), so they sort by start time. The newest 50
  finished runs per project are kept; older ones are pruned.
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
    `version`), and all user-facing reporting for them.
- `internal/config/` — `.cicd.yaml` schema, loading, defaulting, validation.
  Shared with the future VPS agent, which needs to understand
  `images:`/`repository:`/`tags:` to know what to pull.
- `internal/pipeline/` — runs a project's steps in order, stops on first
  failure, then builds/pushes each image.
  - `pipeline.go` — the orchestration state machine.
  - `docker.go` — assembles `docker build`/`docker push` invocations.
  - `runner.go` — the `CommandRunner` seam plus the real `exec`-based
    implementation; `runner_unix.go` adds process-group cancellation.
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
- `internal/display/` — shared formatting of runs (status symbols, ages,
  durations) for the CLI tables and the dashboard.
- `internal/tui/` — the Bubble Tea dashboard (`tui.go`) and its log viewer
  (`logview.go`). It reads the store and launches runs through a callback;
  it never calls the pipeline directly.
- `cmd/j-cicd/manage.go` — the registry/history subcommands (`add`,
  `remove`, `list`, `history`, `logs`, `cancel`, `ui`).
- As more lands (VPS agent, failure-investigation agent), give it its own
  file or package rather than growing an existing one indefinitely.

## Conventions

- Standard Go idioms: `gofmt`/`go vet` clean, camelCase identifiers,
  errors returned and handled explicitly (no panics for expected failures).
- Exported identifiers mean "part of this package's API". Keep helpers
  unexported; `package main` exports nothing.
- All process-level I/O (printing, exiting, signal handling) belongs at the
  command boundary in `cmd/j-cicd`, so the logic underneath stays callable
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
