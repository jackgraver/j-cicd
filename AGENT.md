# VPS deploy agent — design notes

The missing half of j-cicd. `j-cicd run` ends at "pushed to GHCR"; something
on the VPS has to notice and roll the services. This is the design for that
something, before any of it is built.

## Why it lives here and not in vpsm

`vpsm/mini-bot` is already on every host, which makes it the tempting home.
Deliberately not using it:

- mini-bot is a **passive observer**, written in Rust specifically to stay
  small and not compete with what it watches. Giving it the power to stop and
  restart services inverts that: an observer that can take production down is
  no longer safe to deploy casually.
- The blast radius differs. A monitoring agent crash costs you graphs. A
  deploy agent crash costs you the service.
- Deploy needs j-cicd's config model (`.cicd.yaml`, image names, registries).
  vpsm has no reason to know about any of it.

They should stay separate processes with separate credentials. The useful
overlap is the other direction: **the deploy agent emits events, vpsm
collects them**, so a deploy shows up on the same timeline as the CPU spike it
caused. That is a reporting integration, not a merge.

## What the compose file already does for us

`docker-compose.vps.yml` is further along than expected, and the design should
lean on it rather than reimplement it:

- `pull_policy: always` — `up -d` re-pulls without an explicit `pull` step.
- `restart: unless-stopped` — survives reboots.
- **Healthchecks on all three services**, and ordered `depends_on`
  conditions: backend waits for `postgres` healthy, frontend waits for
  `backend` healthy.
- Ports bound to `127.0.0.1` only, so a reverse proxy fronts everything.
- `${BACKEND_IMAGE}` / `${FRONTEND_IMAGE}` are already parameterised — which
  is the hook rollback needs.

So the agent does not need to orchestrate containers or write its own health
logic. `docker compose up -d` plus the existing healthchecks is the deploy.
The agent's real job is narrower: **decide when to run it, confirm it worked,
and undo it if it didn't.**

## Responsibilities

1. Detect that a new image exists for a tracked tag.
2. Deploy it.
3. Verify the stack is healthy afterwards.
4. Roll back to the previous digest if not.
5. Record what happened.

That is the whole scope. Explicitly **not** in it: building, testing,
pushing, migrations (see open questions), or anything requiring the source
tree. The agent never sees a git repo.

## Detecting a new image

Pin to **digests, not tags**. `latest` is a moving pointer; a tag comparison
cannot tell you whether anything changed, and is why the naming mismatch in
[DEPLOY.md](DEPLOY.md) was able to hide.

```
GET https://ghcr.io/v2/<owner>/<image>/manifests/<tag>
Accept: application/vnd.oci.image.index.v1+json
```

Read the `Docker-Content-Digest` response header and compare to the running
digest from `docker inspect`. Differ → deploy. Use `HEAD` rather than `GET`:
same header, no body, and it does not count against pull limits the same way.

Three options for the trigger, in order of preference:

- **Polling** (start here). One `HEAD` per image per interval, 30–60s. No
  inbound ports, no webhook secret, no public endpoint on the VPS. Costs one
  cheap request and is trivially debuggable with `curl`.
- **Push-triggered**, where `j-cicd run` notifies the agent after a
  successful push. Faster, but needs the VPS reachable and authenticated
  inbound. Worth it later; not for v1.
- **Registry webhooks.** GHCR's support is limited and it couples you to the
  registry. Skip.

Polling means a deploy lands up to a minute after the push, which is fine for
this. It also degrades well: if the registry is unreachable the agent simply
does nothing, which is the correct behaviour.

## Deploy sequence

```
lock                                  # flock; never two deploys at once
record current digests                # rollback target, from docker inspect
BACKEND_IMAGE=<repo>@<new-digest> docker compose up -d
wait for healthy (bounded)            # poll docker inspect health status
  healthy  -> write state, emit event, unlock
  timeout  -> roll back, emit failure, unlock
```

Points that matter:

- **Deploy by digest, not tag.** Passing `<repo>@sha256:...` through
  `BACKEND_IMAGE` makes the deploy reproducible and gives rollback something
  concrete to name. The compose file already accepts it.
- **Only restart what changed.** Pass the changed services to `up -d`; don't
  cycle the whole stack, and never touch `postgres` — it is stateful and not
  part of any image update.
- **Take a lock.** Polling plus a slow deploy will otherwise overlap. `flock`
  on a file in the state dir is sufficient.
- **Bound the health wait.** The compose healthchecks define the signal
  (`retries: 5`, `start_period: 10s` on backend), so the agent waits on
  `docker inspect -f '{{.State.Health.Status}}'` rather than re-probing HTTP
  itself. Cap it — roughly `start_period + interval × retries` plus slack,
  around 90s — and treat the timeout as failure.

## Rollback

Rollback is just a deploy of the previous digest, which is why recording it
before touching anything is the critical step:

```
BACKEND_IMAGE=<repo>@<previous-digest> docker compose up -d backend
```

With `pull_policy: always` the old image may have been pruned from the local
cache, so the agent must be able to re-pull by digest. Do not rely on the
local cache holding the rollback target.

Keep rollback dumb and un-clever: one step back to the last digest known
healthy, no chains, no automatic retry of the failed version. If rollback
itself fails, stop and alarm — do not loop. An agent that keeps trying to fix
production unattended is worse than one that stops and says so.

## State

A small JSON file (`/var/lib/j-cicd-agent/state.json`) is enough; no database.

```json
{
  "services": {
    "backend": {
      "repository": "ghcr.io/jackgraver/st-backend",
      "tag": "latest",
      "current_digest": "sha256:c40fbe...",
      "previous_digest": "sha256:9a1f2c...",
      "last_deploy": "2026-11-14T23:16:02Z",
      "last_result": "healthy"
    }
  }
}
```

Survives restarts and answers "what is running and what do I roll back to"
without interrogating Docker. Write atomically (temp file + rename) so a crash
mid-write cannot corrupt it.

## Credentials

The agent needs a GHCR token with **`read:packages` only** — the deploy side
never pushes. Separate token from the build machine's, so a compromised VPS
cannot publish images. This also means the scope error from
[DEPLOY.md](DEPLOY.md) is *correct* behaviour on the VPS; only the build host
should ever have `write:packages`.

Needs Docker socket access, which is root-equivalent on the host. Unavoidable
for something that drives compose, but it argues for keeping the agent small
and separate rather than bolting it onto a tool doing other work.

## Config

Follow the `.cicd.yaml` precedent — declarative, lives next to what it
describes, no flags to remember:

```yaml
# /etc/j-cicd-agent.yaml
project: simple-track
compose_file: /opt/simple-track/docker-compose.vps.yml
env_file: /opt/simple-track/.env
state_dir: /var/lib/j-cicd-agent
poll_interval: 60s
health_timeout: 90s
services:
    - name: backend
      image_var: BACKEND_IMAGE
      repository: ghcr.io/jackgraver/st-backend
      tag: latest
    - name: frontend
      image_var: FRONTEND_IMAGE
      repository: ghcr.io/jackgraver/st-frontend
      tag: latest
```

`image_var` is the mapping from a tracked image to the compose variable the
agent overrides — the one piece of coupling that cannot be inferred.

Run it as a systemd unit with `Restart=always`. Not a cron job: it needs to
hold a lock and own in-flight deploy state.

## Suggested build order

1. `agent status` — read config, query digests, print running vs available.
   Read-only, no deploy. Proves auth and digest comparison, and is the thing
   you will actually want when debugging.
2. `agent deploy --once` — manual, one pass, with rollback. Still no daemon.
3. Health gate and rollback, exercised by deliberately deploying a broken
   image.
4. The polling loop and systemd unit, once the deploy path is trusted.
5. vpsm event reporting.

Keep each step shippable. The first two are independently useful even if the
daemon never gets written.

## Open questions

- **Migrations.** The backend has `cmd/migration` and nothing currently runs
  it on deploy. Does the agent run migrations before starting the new
  backend, or does the backend self-migrate on boot? This is the one genuinely
  unresolved piece — it decides whether rollback is even safe, since a
  rolled-back binary may face a migrated schema. Worth settling before the
  health gate, because "healthy" means little if the schema moved under it.
- **Multi-host.** Config is single-host. Fine for now; a second VPS means
  either one agent per host (simpler) or the collector coordinating (not what
  it is for).
- **Deploy windows.** Should the agent refuse to deploy during certain hours,
  or is "push it and it goes" the intent?

## Dependency on j-cicd

One change needed on the build side: **`j-cicd run` should print the pushed
digest** (and ideally write it to a machine-readable file). Right now it
reports success without saying what it published, so even the push-triggered
variant would have to go ask the registry what just happened. Noted in
[DEPLOY.md](DEPLOY.md).
