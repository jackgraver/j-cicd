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
`.cicd.yaml`, runs its shell steps in order, and if an `image` block is
configured, builds (and optionally pushes) a Docker image for it via the
`docker` CLI. Not yet implemented: the VPS-side deploy helper, and the
stretch-goal failure-investigation agent.

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
image:                  # optional — omit if you don't want a docker stage
  repository: registry.example.com/my-project
  dockerfile: Dockerfile   # default: Dockerfile
  context: .               # default: .
  tags: [latest]           # default: [latest]
```

Registry auth is whatever `docker login` already has configured on the
host — j-cicd just shells out to `docker build`/`docker push`, it doesn't
manage credentials itself.

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
- `docker.go` — shells out to `docker build`/`docker push`.
- `init.go` — writes the starter config template.
- As more lands (VPS agent, failure-investigation agent), give it its own
  file or package rather than growing an existing one indefinitely.

## Conventions

- Standard Go idioms: `gofmt`/`go vet` clean, camelCase identifiers,
  errors returned and handled explicitly (no panics for expected failures).
- Prefer the standard library where practical; only reach for a dependency
  when it clearly earns its keep (e.g. a registry client, YAML parsing).
- No half-finished features behind TODOs — land things when they work end
  to end, even if narrow in scope.
