# VPS deploy agent

`j-cicd run` used to end at "pushed to GHCR"; nothing restarted production.
`j-cicd-agent` is the half that runs on the VPS. It is triggered by the build
host as the **last stage of the run**:

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
   means a deploy never cycles postgres.
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

Interrupting a deploy (Ctrl-C on the build host, or the run's timeout) still
rolls back. A dropped SSH connection doesn't abandon a half-finished rollout:
the agent ignores SIGHUP/SIGPIPE and finishes (or rolls back) on its own.

## State

One JSON file per project, `<state_dir>/<project>.json`, written atomically:

```json
{
  "services": {
    "backend": {
      "repository": "ghcr.io/jackgraver/st-backend",
      "current": "ghcr.io/jackgraver/st-backend@sha256:c40f...",
      "current_tag": "7fc0b4b12345",
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
running, and the next rollback target. `j-cicd-agent status` prints this,
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

- **Migrations.** Still the open question: does the agent run
  `cmd/migration` before starting the new backend, or does the backend
  self-migrate on boot? It decides whether rollback is even safe — a
  rolled-back binary may face a migrated schema.
- **Restricting the SSH key.** Today the deploy key is a normal login. A
  `command="..."` forced command in `authorized_keys` that only allows
  `j-cicd-agent deploy` would limit a stolen build-host key to "deploy a
  digest of a configured repository". The agent's strict argument validation
  is the groundwork for that.
- **vpsm event reporting**, so deploys appear on the same timeline as the
  metrics they affect.
- **Multi-host.** One `deploy:` host per project for now.
