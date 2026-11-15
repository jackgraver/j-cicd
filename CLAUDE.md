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
root. Not yet implemented: the VPS-side deploy helper, and the stretch-goal
failure-investigation agent.

j-cicd is project-agnostic by design: it takes a project directory and that
project's own `.cicd.yaml` as input. `simpletrack` is just the first project
pointed at it — nothing in the tool assumes a specific project, and running
it against a second or third project just means writing another config.

## Config format (`.cicd.yaml`, lives in the target project's repo)

```yaml
name: my-project       # required
steps:                 # optional list of shell commands, run in order
  - name: build
    run: go build ./...
  - name: test
    run: go test ./...
    dir: subdir         # optional, relative to the project dir
images:                 # optional list — omit if there's no docker stage
  - name: backend        # optional, just a label for logs
    repository: ghcr.io/owner/my-project-backend
    dockerfile: backend/Dockerfile   # default: Dockerfile
    context: backend                 # default: .
    tags: [latest]                   # default: [latest]
  - name: frontend
    repository: ghcr.io/owner/my-project-frontend
    dockerfile: frontend/Dockerfile
    context: frontend
```

Each entry in `images:` is built and (optionally) pushed independently, so
multi-service projects just list one entry per service/Dockerfile.

Registry auth is whatever `docker login` already has configured on the
host — j-cicd just shells out to `docker build`/`docker push`, it doesn't
manage credentials itself. ghcr.io (GitHub Container Registry) works fine
this way; see the project owner's own notes for the one-time
`docker login ghcr.io` setup.

`j-cicd init` scans the project directory for Dockerfiles (skipping
`node_modules`/`vendor`/`dist`/`build`/dotfiles, up to 4 levels deep) and
generates one `images:` entry per Dockerfile found, named after its parent
directory. If the project's git `origin` remote points at GitHub, the
generated `repository:` defaults to `ghcr.io/<owner>/<project>-<service>`;
otherwise it falls back to a `registry.example.com/...` placeholder that
needs editing by hand.

## Nix

A `flake.nix` packages j-cicd with `buildGoModule` (NixOS is the primary dev
machine). Useful commands:

- `nix build .#default` — build the binary into `./result/bin/j-cicd`.
- `nix profile install .#default` — install it into the user profile so
  `j-cicd` is on `PATH` globally (re-run after dependency/version changes).
- `nix develop` — drop into a shell with `go` and `docker` available.

If `go.mod`/`go.sum` change (new or updated dependency), `vendorHash` in
`flake.nix` needs to be recomputed: set it to `pkgs.lib.fakeHash`, run
`nix build .#default`, and copy the "got:" hash from the mismatch error back
into `vendorHash`.

## CLI

- `j-cicd init [project-dir]` — scaffold a starter `.cicd.yaml`.
- `j-cicd run [project-dir] [--config path] [--no-push]` — run the pipeline.
- `j-cicd version`

## Code structure

- `main.go` stays minimal: it only calls `appStart()`.
- `app.go` owns application startup and dispatches `os.Args` to a subcommand.
- `commands.go` — CLI flag parsing per subcommand (`run`, `init`, `version`).
- `config.go` — `.cicd.yaml` schema and loading/validation.
- `pipeline.go` — runs a project's steps in order, stops on first failure.
- `docker.go` — shells out to `docker build`/`docker push`, once per image.
- `detect.go` — project inspection used by `init` (Dockerfile scan, GitHub
  remote detection, go.mod/package.json detection).
- `init.go` — assembles and writes a starter config using `detect.go`.
- As more lands (VPS agent, failure-investigation agent), give it its own
  file or package rather than growing an existing one indefinitely.

## Conventions

- Standard Go idioms: `gofmt`/`go vet` clean, camelCase identifiers,
  errors returned and handled explicitly (no panics for expected failures).
- Prefer the standard library where practical; only reach for a dependency
  when it clearly earns its keep (e.g. a registry client, YAML parsing).
- No half-finished features behind TODOs — land things when they work end
  to end, even if narrow in scope.
