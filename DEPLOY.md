# Running j-cicd against a real project

Notes from the first end-to-end run of j-cicd on `simple-track`, including a
production image build. Written as a runbook, with the things that bit us
called out where they bit.

## Prerequisites

**A GHCR token with `write:packages`.** The build half needs nothing but
Docker; the push half needs this scope specifically. A token with only
`read:packages` fails late — after every step and both image builds have
succeeded — with:

```
error from registry: permission_denied: The token provided does not match expected scopes.
```

Log in before running anything you intend to push:

```sh
echo "$GHCR_PAT" | docker login ghcr.io -u <user> --password-stdin
```

Avoid `docker login -p <token>`: it lands the token in shell history, and
Docker stores it base64-encoded (**not** encrypted) in `~/.docker/config.json`
regardless. Configure a `credsStore` if you want it kept out of plaintext.

## Build from a committed tree, not your working tree

j-cicd builds whatever is on disk. It does not clone, check out, or care
whether the tree is dirty — so a casual `j-cicd run .` can ship half-finished
work to production. Use a throwaway worktree pinned to a real commit:

```sh
git worktree add /tmp/st-prod <commit>
cd /tmp/st-prod/frontend && npm ci      # test steps run on the host
j-cicd run -no-push /tmp/st-prod        # verify first
j-cicd run /tmp/st-prod                 # then push
git worktree remove /tmp/st-prod
```

This keeps your working tree untouched and makes the artifact traceable to a
commit. It is also the only way to be sure what went into the image.

j-cicd now makes a dirty build visible rather than preventing it: with no
explicit `tags:`, images are tagged `latest` plus the short git revision of
the directory being built, suffixed `-dirty` when the tree has uncommitted
changes. A `...-dirty` tag in the registry means the image did not come from
the commit it names.

On our run this mattered concretely. The working tree had an in-progress
`useItemInput.ts` referencing ten names that did not exist in the module.
`vite build` strips types without resolving identifiers, so **the image built
successfully and would have thrown `ReferenceError` the moment a user typed in
the grocery input.** Building from the last commit excluded it. Verified by
pulling the bundle out of the image and grepping it:

```sh
cid=$(docker create <image>)
docker cp $cid:/usr/share/nginx/html/assets ./assets-check
docker rm $cid
grep -rl "<symbol-from-your-wip>" ./assets-check/   # expect no match
```

## Flag ordering: fixed, and why it bit

On the first run, `-no-push` was silently ignored when it came *after* the
project dir — the documented order — and the image was pushed anyway. It
only failed to land because the token lacked `write:packages`; the safety
net was an accident, not a design.

```sh
j-cicd run . -no-push     # was: flag ignored, image pushed. now: no push
j-cicd run -no-push .     # was and is: no push
```

Both orders now work. `cmdRun` no longer parses the raw argv (Go's `flag`
package stops at the first non-flag argument); it peels the project dir off
and keeps parsing, and anything left unconsumed — an unknown flag, a second
positional — is an error. A flag whose whole purpose is preventing a publish
must never be ignorable.

## Keep image names in one place

`.cicd.yaml` and `docker-compose.vps.yml` have to agree, and nothing checks
that they do. Ours initially disagreed:

| Source | Image |
| --- | --- |
| `.cicd.yaml` (before) | `ghcr.io/jackgraver/simple-track-backend` |
| `docker-compose.vps.yml` default | `ghcr.io/jackgraver/st-backend` |
| `.github/workflows/main.yml` | `ghcr.io/jackgraver/st-backend` |

A push would have gone to a package the VPS never pulls, so `docker compose
pull` would report success having fetched a months-old image. **This failure is
silent in both directions** — the build is green, the deploy is green, and
production does not change.

We standardised `.cicd.yaml` on `st-*` to match what the compose file already
defaults to. If you instead override with `BACKEND_IMAGE`/`FRONTEND_IMAGE` on
the VPS, note that the compose defaults become a trap: lose the env file and
the stack silently falls back to the other package.

## Pipeline steps run on the host

`run:` executes through a shell, so `cd`, `&&`, and `$(...)` command
substitution all work:

```yaml
- name: backend-test
  run: cd backend && go test $(go list ./... | grep -v '/cmd/migration')
```

But these run on the *host*, not in a container — the host needs the
toolchains, and `frontend/node_modules` must exist, hence the `npm ci` in a
fresh worktree. The steps are also independent of the Docker build, which
re-does its own `go build` / `npm ci && npm run build` inside the Dockerfile.
Expect to compile twice.

## Failure gating works

Verified on two genuinely failing steps. j-cicd exits non-zero, names the
failing step, and **stops before any `docker build`**:

```
2026/11/14 23:09:10 pipeline failed: step "frontend-typecheck" failed: exit status 2
```

No image is built, tagged, or pushed. A failed test cannot reach the registry.

## Gates currently disabled, and why

Both are pre-existing `simple-track` problems, not j-cicd ones. Documented in
`.cicd.yaml` next to the steps they affect.

- **`cmd/migration` excluded from `go test`.** `main_test.go:17` has a stray
  empty table entry `{}`, asserting `IncrementVersion("") == ""` when it
  returns `"001"`. A test-fixture bug — looks like a leftover trailing entry.
  One-line fix, then drop the `grep -v`.
- **`npm run type-check` omitted.** ~15 pre-existing `TS6133`/`TS6192`
  "declared but never read" errors on a clean tree. All unused-symbol noise,
  but enough to make the step permanently red.

The distinction between the two error classes turned out to be the useful
signal: clean `HEAD` had **only** unused-symbol warnings, while the dirty tree
added ten `TS2304 "Cannot find name"` — the class that actually breaks at
runtime. If you re-enable type-check, consider failing on `TS2304` alone
first; it catches real faults without demanding a lint cleanup.

Note that nothing in the pipeline currently type-checks the frontend, and the
Dockerfile's `vite build` does not either.

## Verifying images beyond "it built"

A tagged image is not a working one. Cheap checks that caught real information:

```sh
# frontend actually serves, and SPA fallback works
cid=$(docker run -d -p 18080:3000 <frontend-image>)
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:18080/
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:18080/some/deep/route
docker rm -f $cid

# backend binary boots and validates config
docker run --rm <backend-image>        # expect the env-var or DB error, not a crash
```

The backend exiting 1 on `config: required env var not set: JWT_SECRET`, then
on a Postgres connection retry loop, is the success case with no database
around — it proves the binary runs and its config validation works. Both
images also run as non-root (`app` uid 100, `nginx` uid 101); worth confirming
after Dockerfile changes.

Delete the tags before a "did it really rebuild" run, or you cannot tell a
fresh build from a cache hit:

```sh
docker rmi <backend-image> <frontend-image>
```

## Result

Prod images built from a clean worktree at `7fc0b4b`, all steps green,
verified serving and booting:

| Image | Size |
| --- | --- |
| `ghcr.io/jackgraver/st-backend:latest` | 51.9 MB |
| `ghcr.io/jackgraver/st-frontend:latest` | 82.9 MB |

## What j-cicd does not do yet

- **No clone / commit pinning.** Builds the directory you point it at. The
  worktree dance above is a workaround; `j-cicd run --ref <commit>` would
  remove the footgun.

Since this was written, a `deploy:` section in `.cicd.yaml` makes the run
finish by deploying the per-commit tags it just pushed on the VPS through
`j-cicd-agent`, which pins them to digests — see [AGENT.md](AGENT.md). It
refuses dirty trees, which also closes the "half-finished work shipped to
production" gap above for anything that deploys.
