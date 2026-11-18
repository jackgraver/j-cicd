# VPS deploy agent

`j-cicd run` used to end at "pushed to GHCR"; nothing restarted production.
`j-cicd-agent` is the half that runs on the VPS. It is triggered by the build
host as the **last stage of the run**. It deploys compose stacks from images
(below) and, for projects that ship binaries, installed files and the
systemd units that run them ([Files projects](#files-projects-binaries-and-systemd-services)):

```
build host (j-cicd run)                         VPS (j-cicd-agent)
───────────────────────                         ──────────────────
refuse up front if tree is dirty / no commit tag
steps -> docker build -> docker push
ssh deploy@vps j-cicd-agent deploy  ───────────► lock project
    <project> repo:<commit>                      pull repo:<commit>, resolve to repo@sha256:...
                                                 compose up -d --no-deps <services whose digest changed>
                                                 wait for compose healthchecks
                                                   healthy  -> record, exit 0
                                                   failed   -> roll back to last healthy, exit 1
run passes/fails on the agent's exit  ◄───────── (output streams back over ssh)
```

So a single `j-cicd run` answers "did my change reach production, and is it
healthy". A deploy that fails its health check rolls itself back *and* fails
the run.

## Why push-triggered over SSH (and not polling)

The first design (polling the registry every 60s) was dropped in favour of
the build host telling the VPS directly, because:

- **The run knows the outcome.** With polling, `j-cicd run` goes green at
  push time and the deploy result lives in some other log. Now a broken
  release fails the same run that built it.
- **SSH is the transport.** No daemon, no open port, no webhook secret on the
  VPS: the agent is a plain CLI invoked per deploy, authenticated by the SSH
  key the build host already has. `BatchMode=yes` means it fails rather than
  prompting.
- **Exactly what was built gets deployed.** The build host sends the
  per-commit tag it just pushed, never `latest`.

## Tags vs digests

A **tag** (`latest`, `7fc0b4b12345`) is a name pointing at an image, and can
be re-pointed by any later push. A **digest** (`sha256:c40f...`) is the
registry's hash of the image contents, so it always means exactly one
image. The build host sends the per-commit tag because it's readable and is
what the run just pushed; the agent resolves it to a digest the moment it
pulls, and from then on uses only the digest — to start the container, to
record what's running, and to roll back. So logs and `status` show
commits, while what actually runs can't change underneath you.

The build host refuses to deploy (before building anything) when there's
no per-commit tag to send, or when the tree has uncommitted changes — a
`<commit>-dirty` image isn't what that commit contains, and two different
dirty builds share the tag. `j-cicd run --deploy-dirty` overrides the
latter. `j-cicd-agent deploy` also accepts a digest directly
(`repo@sha256:...`), which is handy for a manual rollback.

## Why it lives here and not in vpsm

`vpsm/mini-bot` is a passive observer and should stay one: an observer that
can take production down is no longer safe to deploy casually, and a deploy
agent crash costs you the service where a monitoring crash costs you graphs.
They stay separate processes; the useful integration is the deploy agent
*emitting* events for vpsm to collect (not built yet).

## What the agent does

All of it is driven by the compose file — the agent doesn't orchestrate
containers or write its own health logic:

1. **Validates the request.** Every reference must be `repo:<tag>` (not
   `latest`) or `repo@sha256:<digest>`, and every repository must map to a
   service in the agent config. An
   unknown repository is an *error*, which turns the silent image-name
   mismatch described in [DEPLOY.md](DEPLOY.md) into a loud one.
2. **Locks the project** (`flock` on `<state_dir>/<project>.lock`), so two
   runs can't deploy over each other; the second waits.
3. **Pulls first**, separately, then resolves each tag to the digest docker
   recorded for it. If the registry or tag is unavailable, nothing has been
   touched and there is nothing to roll back.
4. **Skips services already running that digest**, so re-running a pipeline
   is a no-op on the VPS.
5. **`docker compose up -d --no-deps <changed services>`** with each image
   variable (`BACKEND_IMAGE=repo@sha256:...`) set in the environment. Every
   other tracked service's variable is pinned to its recorded release too, so
   compose never falls back to the compose file's default tag. `--no-deps`
   means a deploy never cycles postgres. Just before this, the project's
   **`pre_deploy`** command runs (if set) — after the pull, so a release
   that can't be pulled doesn't cost a backup, and before anything
   changes, so a failed backup cancels the deploy with nothing touched.
   After `pre_deploy`, the project's **`pre_swap`** command runs (if set,
   and only when its service is among those changing) inside that
   service's *new* image — see [pre_swap](#pre_swap-migrations-and-the-like).
6. **Waits for healthy** using the compose healthchecks (`docker inspect`
   health status), bounded by `health_timeout`. `unhealthy`, `exited`,
   `dead` or `restarting` fail immediately; a service with no healthcheck is
   accepted once `running`, with a warning.
7. **On failure, rolls back** one step to the last release that passed its
   health check, using the same pull/up/health sequence. It never retries the
   failed release and never chains rollbacks. If the rollback fails too, or
   there is no previous release (first deploy), it says so in capitals and
   exits non-zero — an agent that keeps trying to fix production unattended
   is worse than one that stops.

## pre_swap: migrations and the like

A command the *new* release needs run once, before it serves traffic,
e.g. simple-track's `/app/migrate up`. The agent decides when it runs; the
image decides what it does, so the agent stays migration-agnostic.

```yaml
    pre_swap:
      service: backend                 # run in this service's NEW image
      command: ["/app/migrate", "up"]  # exec form, no shell
      timeout: 10m                     # optional; else only the deploy's timeout
```

Deploy order: pull → `pre_deploy` (backup) → **`pre_swap`** → `up -d` →
health check → rollback on failure. It runs as

```
env BACKEND_IMAGE=<repo@sha256:new> FRONTEND_IMAGE=<pinned> ... \
  docker compose -f <compose_file> --env-file <env_file> \
  run --rm --no-deps -T --name <project>-pre-swap-<time> backend /app/migrate up
```

so it gets the service's environment, networks and env file exactly as
the real deploy does (and the release's compose file, when one is sent).
`--no-deps` means it never starts or recreates anything else: if the
database isn't running, the command fails rather than compose starting
it. Output streams into the deploy log like every other command's.

- **Non-zero exit or timeout aborts the deploy with no container
  changed**, recorded as `pre_swap failed, nothing changed`. On timeout
  (or Ctrl-C) the container is stopped with `docker stop -t 30` (SIGTERM,
  then SIGKILL), since killing the compose client alone leaves it running;
  `docker rm -f` is the fallback if that fails.
- **The command should exit promptly on SIGTERM and cancel what it's
  doing.** Killing it isn't enough: Postgres doesn't notice a client that
  died mid-statement until that statement finishes, so a SIGKILLed
  migration keeps running, holding its table locks against the live app,
  before finally rolling back. (Found in acceptance testing: a timed-out
  `ALTER TABLE` + `pg_sleep` kept an `AccessExclusiveLock` after the deploy
  had "aborted".) simple-track's `migrate` cancels its context on SIGTERM,
  and pgx then sends Postgres a cancel request.
- **It is never undone.** If the new release then fails its health check,
  images are rolled back as usual and the command's effects stay. The
  automatic rollback doesn't run it again. For migrations that means the
  previous release must tolerate a schema one step ahead of it (simple-track's
  does: additive changes only, and it logs a warning). Undoing a schema
  change is a deliberate, manual `/app/migrate down`.
- **It is skipped when its service isn't changing** (e.g. a
  frontend-only release).
- **A manual rollback runs it** in the *older* image, because a rollback
  is an ordinary deploy of the previous release. For `migrate up` that's a
  no-op (the database is already ahead).

## Manual rollback

For a problem noticed after a deploy passed its health check:
`j-cicd rollback <project>` on the build host (or `R` in the dashboard)
runs `j-cicd-agent rollback <project>` over the same ssh path. The agent
puts each service **the most recent successful deploy changed** back on
the release it replaced (services that deploy didn't touch stay put),
through the normal deploy sequence: pull, `pre_deploy` (so it's backed up
too), up, health check, and an automatic return to the current release if
the old one doesn't come up healthy. It's recorded in history as a run of
kind `rollback`. Running it again undoes it, since the release rolled back
from becomes the previous one; to go further back, deploy an older commit
tag or digest with `j-cicd-agent deploy`.

**Only images are rolled back, never the database.** If the release being
replaced changed the schema, the older binary meets the newer schema. That
is safe for purely additive changes (new tables, nullable columns) and not
otherwise; the `pre_deploy` backup is the way back for the rest.

Interrupting a deploy (Ctrl-C on the build host, or the run's timeout) still
rolls back. A dropped SSH connection doesn't abandon a half-finished rollout:
the agent ignores SIGHUP/SIGPIPE and finishes (or rolls back) on its own.

## Files projects: binaries and systemd services

Not everything ships as an image. A CLI on the build host, or a static
binary run by systemd on the VPS (vpsm's mini-bot), is a **files project**:
the agent config lists `files:` instead of `compose_file`/`services:`, and
the build host's `.cicd.yaml` lists `artifacts:` instead of `images:`. The
deploy gets the same guarantees as a compose one, in the same order:

```
build host                                    agent (VPS, or this machine)
──────────                                    ────────────────────────────
refuse up front if tree is dirty / no commit
steps -> artifacts must exist -> sha256 each
ssh host j-cicd-agent deploy <project>  ────► lock project
    name@sha256:<hex>... --artifacts-stdin     receive the tar, store each file under its hash,
    --revision <commit>   (+ tar on stdin)       refuse any that doesn't hash to its ref
                                               preflight: in the store, target dir writable,
                                                 unit known to systemd       (else: nothing changed)
                                               keep what's on disk if the agent didn't put it there
                                               pre_deploy
                                               install: write beside, fsync, rename over (atomic)
                                               systemctl restart <unit>; must stay active on the
                                                 same main pid, restart counter unchanged, for settle
                                               check: command must exit 0
                                                 ok     -> record, exit 0
                                                 failed -> put the previous files back, restart,
                                                           settle, check; exit 1
```

- **Content-addressed, like digests.** A release of a file is its sha256.
  Every release the agent installs is kept in
  `<state_dir>/<project>.files/<sha256>/<name>` (current and previous;
  older ones are pruned), and is re-hashed on its way into place, so a
  corrupted store copy is refused rather than installed. Rollback needs
  nothing from the build host, and `j-cicd-agent deploy <project>
  name@sha256:<hex>` (no stdin) re-deploys any stored release.
- **Atomic replace, not `cp` or `go install`.** The new file is written
  next to the target and renamed over it. A running process keeps its old
  inode, nothing ever executes half a file, and the agent can replace its
  own binary (or j-cicd's) mid-run.
- **Even the first deploy can be undone.** A file at the target path that
  the agent didn't install (what was there before the first deploy, or a
  hand-copied hotfix since) is stored and recorded as the release to roll
  back to, shown as `untracked` in `status`, instead of being lost.
- **"Active" is not "healthy".** A service that crashes on start is
  restarted by systemd and reads `active` again a second later. After its
  restart the unit must stay active on one main PID, with `NRestarts`
  unchanged, for `settle` (default 10s), within `health_timeout`; a
  `failed` unit or one in `auto-restart` fails at once. The unit's last
  journal lines go into the deploy log on failure.
- **check:** is an exec-form command run after the restart (or right after
  the install, for a CLI with no unit), with `J_CICD_PROJECT` and
  `J_CICD_REVISION` set. Exit non-zero and the deploy rolls back. Typical:
  the binary's `version` must print the revision it was built from.

```yaml
projects:
  - name: mini-bot
    files:
      - name: mini-bot                # = artifacts: name in .cicd.yaml
        path: /opt/vpsm/bin/mini-bot  # absolute; its directory must be writable by the agent
        mode: "0755"                  # optional, default 0755
    systemd:                          # optional
      unit: mini-bot.service
      scope: system                   # default; "user" for systemctl --user
      settle: 15s                     # default 10s
    # check: ["/opt/vpsm/bin/mini-bot", "--version"]   # optional exec-form command
    health_timeout: 60s               # bounds settle + check, as for compose
    pre_deploy: ...                   # optional, as for compose (run from the agent's cwd)
```

**Privileges.** The agent stays an unprivileged user. Two things need
care for a service that runs as root:

1. The binary's directory must be writable by the agent (the atomic
   rename needs it), so it can't be `/usr/local/bin`. Use a directory the
   deploy user owns (`/opt/vpsm/bin`), point the unit's `ExecStart` there,
   and symlink `/usr/local/bin/<name>` to it if it should be on `PATH`.
2. Restarting a system unit: a non-root agent runs `sudo -n systemctl
   restart <unit>` (never prompting), so add exactly that to sudoers:
   `deploy ALL=(root) NOPASSWD: /usr/bin/systemctl restart mini-bot.service`.
   vpsm's `install.sh <host-id> <pull-user> <deploy-user>` sets up both.

Either way the deploy user then decides what root executes, so it should
be a user that could already become root (docker group, sudo) — that is
true of any compose deploy user too.

### Local deploys

`deploy: {local: true}` runs the agent on the build host itself
(`sh -c "<command> deploy ..."`, no ssh), with its own config — which is
how j-cicd installs itself and the vpsm CLIs into `~/.local/bin`:

```yaml
# ~/.config/j-cicd-agent.yaml on the build host
state_dir: /home/you/.local/state/j-cicd-agent
projects:
  - name: j-cicd
    files:
      - {name: j-cicd, path: /home/you/.local/bin/j-cicd}
      - {name: j-cicd-agent, path: /home/you/.local/bin/j-cicd-agent}
    check: ["sh", "-c", "for b in j-cicd j-cicd-agent; do /home/you/.local/bin/$b version | grep -qF -- \"$J_CICD_REVISION\" || { echo \"$b does not report $J_CICD_REVISION\" >&2; exit 1; }; done"]
```

and in the project's `.cicd.yaml`:
`deploy: {local: true, command: J_CICD_AGENT_CONFIG=$HOME/.config/j-cicd-agent.yaml j-cicd-agent}`.
The binary that runs the deploy is the *installed* agent, so the first
agent that understands artifacts has to be installed by hand once
(`GOBIN=~/.local/bin go install ./cmd/...`); after that each release
installs the next. The same goes for the agent on the VPS
(`.cicd-vps-agent.yaml`): copy it by hand once, and from then on
`j-cicd run j-cicd-vps-agent` has the running agent replace itself.

## State

One JSON file per project, `<state_dir>/<project>.json`, written atomically:

```json
{
  "services": {
    "backend": {
      "repository": "ghcr.io/jackgraver/st-backend",
      "current": "ghcr.io/jackgraver/st-backend@sha256:c40f...",
      "current_tag": "7fc0b4b12345",
      "current_since": "2026-11-15T22:57:19Z",
      "previous": "ghcr.io/jackgraver/st-backend@sha256:9a1f...",
      "previous_tag": "51d2a0e9c3f7",
      "last_attempt": "ghcr.io/jackgraver/st-backend:7fc0b4b12345",
      "last_deploy": "2026-11-15T22:57:19Z",
      "last_result": "healthy"
    }
  }
}
```

`current` is the last release that passed its health check: what should be
running, and the next rollback target. `current_since` is when it was
deployed: the services sharing the latest one are what a manual rollback
reverts. `j-cicd-agent status` prints this,
showing each release as its commit tag plus a short digest.

## Setup

### On the VPS

1. **Install the binary.** Build it on any machine with Go:
   `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o j-cicd-agent ./cmd/j-cicd-agent`,
   then copy it to `/usr/local/bin/j-cicd-agent`.
2. **A deploy user** that can run docker (e.g. in the `docker` group). Docker
   socket access is root-equivalent; that's unavoidable for something that
   drives compose, and is why the agent is kept small and separate.
3. **Registry login with `read:packages` only**, as that user:
   `docker login ghcr.io`. The VPS never pushes, so a compromised VPS can't
   publish images; only the build host holds `write:packages`.
4. **Parameterise the compose file's images**, e.g.
   `image: ${BACKEND_IMAGE:-ghcr.io/jackgraver/st-backend:latest}`.
   `docker-compose.vps.yml` in simple-track already does this.
5. **`/etc/j-cicd-agent.yaml`** (or point `$J_CICD_AGENT_CONFIG` / `--config`
   elsewhere):

   ```yaml
   state_dir: /var/lib/j-cicd-agent   # default; must be writable by the deploy user
   projects:
     - name: simple-track             # matches deploy.project (default: name:) in .cicd.yaml
       compose_file: /opt/simple-track/docker-compose.vps.yml
       env_file: /opt/simple-track/.env   # optional, passed as --env-file
       health_timeout: 90s            # default 90s; ~ start_period + interval × retries + slack
       timeout: 15m                   # default 15m; bounds pull + up + health wait
       pre_deploy: /usr/local/bin/backup-db  # optional; sh -c from the compose dir, before
                                      # containers change; non-zero exit cancels the deploy
       pre_swap:                      # optional; see "pre_swap" above
         service: backend
         command: ["/app/migrate", "up"]
         timeout: 10m
       services:
         - name: backend              # compose service name
           image_var: BACKEND_IMAGE   # variable the compose file interpolates
           repository: ghcr.io/jackgraver/st-backend
         - name: frontend
           image_var: FRONTEND_IMAGE
           repository: ghcr.io/jackgraver/st-frontend
   ```

   Unknown keys are errors, like `.cicd.yaml`. A repository may map to only
   one service, and an image variable to only one service.

### On the build host

1. An SSH key that can log in as the deploy user non-interactively, and the
   VPS's host key already in `known_hosts` (`BatchMode=yes` won't prompt to
   accept it). Ports, keys and jump hosts go in `~/.ssh/config`.
2. A `deploy:` section in the project's `.cicd.yaml`:

   ```yaml
   deploy:
     host: deploy@vps.example.com    # required; user@host or ~/.ssh/config alias
     command: j-cicd-agent           # optional; e.g. /usr/local/bin/j-cicd-agent
     project: simple-track           # optional; defaults to name:
     images: [backend, frontend]     # optional; defaults to every images: entry
     timeout: 10m                    # optional; defaults to the top-level timeout
   ```

`j-cicd run --no-deploy` builds and pushes but skips the deploy;
`--no-push` skips it too, since the VPS can only pull what was pushed. Leave
`tags:` out of deployed images (the default tags include the commit), and
commit before running — or pass `--deploy-dirty`.

## Not done yet

- **Restricting the SSH key.** Today the deploy key is a normal login. A
  `command="..."` forced command in `authorized_keys` that only allows
  `j-cicd-agent deploy`/`rollback` would limit a stolen build-host key to "deploy a
  digest of a configured repository". The agent's strict argument validation
  is the groundwork for that.
- **vpsm event reporting**, so deploys appear on the same timeline as the
  metrics they affect.
- **Multi-host.** One `deploy:` host per project for now.
- **Shipping unit files.** A files deploy replaces binaries; changes to a
  systemd unit itself still go through the host's own install script.
