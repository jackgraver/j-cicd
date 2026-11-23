# j-cicd guide for AI agents

j-cicd builds, tests, pushes and deploys registered projects. This guide is
compiled into the binary you are running, so it matches this version. Prefer
asking the tool over trusting remembered details: `j-cicd run --plan`,
`j-cicd <command> -h`, `j-cicd logs`.

## Short version

`j-cicd guide --short` prints just this section; the rest is by topic.

- Deploy: `j-cicd deploy <name> --brief` plans, then runs pinned to the
  planned commit. To ask the user first, `j-cicd run <name> --plan`.
- Ask the user before a production deploy unless they already said to deploy.
- Before merging: `j-cicd check <name> --ref <branch> --brief` runs the
  config's steps on that branch, nothing else.
- `--brief` prints one line on success (`ok <name> <rev> deployed verified
  43s`) and only the failure object on failure. Read `failure.tail` first.
- Failures, `logs`, `history --last`: see "When a run fails". Undo: `rollback`.
- Never build images by hand or ssh to the VPS to deploy.

## Rules

- Deploy through j-cicd only. Never build images by hand or ssh to the VPS to
  deploy (image projects), or copy binaries into place by hand (files
  projects).
- Ask the user before a production deploy (`run` that pushes and deploys)
  unless they already told you to deploy.
- What a run builds depends on the project's `branch:`. With `branch:` set it
  builds that branch's latest *commit* in a clean checkout: uncommitted
  changes are left out (and don't block), so commit what should ship. Without
  `branch:` it builds the working tree, and a dirty tree is refused for a
  deploy (`--deploy-dirty` overrides). `--plan` states which applies.
- With `remote: origin` next to `branch:`, a run first fetches that branch and
  builds the *remote's* copy, so a PR merged on GitHub ships without anyone
  pulling. Don't fast-forward or touch the main checkout to deploy; its state
  doesn't matter. A failed fetch fails the run (it never falls back to a stale
  local branch). Local commits not on the remote are left out and reported as
  a warning. Without `remote:` the local branch is built, and `--plan` warns
  when it's behind its upstream (as of the last fetch).

## Deploy workflow

The standard invocation for a release-branch project (`branch:` set), so
there is nothing to decide each time (without `branch:`, drop `--commit`):

    j-cicd run <name> --plan            # review the commit, steps, deploy host
    j-cicd run <name> --commit <rev> --json   # <rev> = the plan's revision

When the user has already said to deploy, do both in one call:
`j-cicd deploy <name> [--brief|--json]` resolves the plan once, prints its key
lines (commit, images or files, host) to stderr, and runs pinned to that
commit. It is as safe as the two steps; use `run --plan` when you need to
ask the user in between.

`--brief` (on `run`, `deploy`, `rollback`, `check`) keeps the output small: one
stdout line when the run passes, e.g. `ok simple-track 7b2eed0ec256 deployed
verified 43s`, and only a failure object (`failure`, `next`, `exit_code`)
when it doesn't. `--json` is the full record.

`--commit` ties the run to what you reviewed: it builds exactly that commit
(it must be in the release branch's history, and needs `branch:`), even if
the branch has moved since the plan. Use `--json` rather than `--quiet`: the
result object says whether it passed, what stage failed, the digests and
whether it was deployed. A `verify:` stage already checks the live site, so
don't add a curl of your own.

1. `j-cicd run <name|dir> --plan` shows the commit, steps, images, deploy
   host and verify URLs without doing anything. Exit 1 means a PROBLEM
   would refuse the run. `--plan --json` is machine-readable.
2. `j-cicd run <name|dir>` runs the pipeline. Without a terminal it prints
   one line per stage and the failing stage's last lines. `-v` streams
   everything, `--quiet` prints only the final status, `--json` prints one
   result object on stdout.
3. `--no-push` builds only; `--no-deploy` pushes images (image projects)
   without deploying. `--deploy-dirty` overrides the dirty-tree refusal;
   don't use it unasked.
4. Only one run per project at a time: starting another while one is going
   fails on the per-project lock. `j-cicd logs <name> -f` follows a live run;
   `cancel` stops it.

## Catch failures before merging

Lint, tests and the build run inside the deploy, so a failure found there is
found late. Run the same steps before merging. For a project without
`branch:`, `j-cicd run <name> --no-push --no-deploy` builds the working tree
(steps, images, artifacts) without pushing or deploying, so it checks the
pipeline end to end. With `branch:` set it builds the release branch, not
your feature branch, so it can't check a PR. Use `j-cicd check <name>`: it runs
only the `steps:` from `.cicd.yaml` (no images, push or deploy) against the
directory as it is on disk, or with `--ref <branch|sha>` against a clean
checkout of that commit using the config committed there. It is recorded in
history as a `check` run, and takes `--brief`, `--json` and `-v` like `run`.

## Project kinds

- **Image projects** (`images:`): built and pushed to a registry, then the
  VPS agent pulls them over ssh (`deploy.host`).
- **Files projects** (`artifacts:`): binaries listed in `.cicd.yaml` are
  hashed and sent to the agent, installed atomically, their systemd unit
  restarted and checked. `deploy.local: true` does this on this machine, with
  no ssh (j-cicd installs itself that way). `--no-deploy` still builds and
  hashes, but nothing is installed.

`--plan` shows which kind and where it deploys.

## When a run fails

`run --json` prints one object even on failure. Read it before the logs:

- `failure` says what stopped the run: `kind` (step, build, push, deploy,
  verify, docker-check, artifacts, config, cancelled), `stage` (the step or
  image name), `exit_code`, `timed_out`, and `tail`, the last lines of that
  stage's output with colour codes and progress redraws removed. Often that
  is the whole diagnosis.
- A `cancelled` failure still names the stage that was interrupted.
- `stages` lists every stage with its status and seconds, so you can see what
  passed. `exit_code` is j-cicd's own exit status.
- `next` has ready-to-run commands (the failing stage's log, a rollback if
  something is already deployed).
- With no run recorded (unknown project, bad flag, busy project) the object is
  `{"status":"error","error_kind":"usage|config|not_found|busy|internal",
  "error":...,"exit_code":N}`.
- `history --json` has the same `failure` and `stages` for earlier runs;
  `history <name> --last` (or `--limit N`) keeps it to the latest.

More output, only if the tail isn't enough:

- `j-cicd logs <name|run-id> --tail 60` for the end of the log; `history`
  lists recorded runs; `cancel` stops a run in progress.
- The log is marked per stage (`== stage step test ==`). Read one stage with
  `logs <name> --stage test` (a stage's name, exactly, or `step test` /
  `build app`), search with `--grep 'FAIL|panic' -C 3`. Off a
  terminal, colour codes and progress-bar redraws are stripped (`--raw` keeps
  them). `--stage` and `--grep` can't be combined with `-f`.
- A failure at the verify stage means the release IS running but the public
  site isn't serving it (yet). Investigate before rolling back.
- The agent rolls a failed deploy back by itself (on the VPS, or locally for
  `deploy.local`); verify failures never trigger a rollback.

## Rollback

`j-cicd rollback <name>` reverts the last deploy: images or files, never the
database. It never happens automatically. Running it again undoes it.

Because the database stays as it is, a release with a migration is not
undone by a rollback: before rolling one back, look at the project's
pre-deploy backup (the agent's `pre_deploy` command, e.g. a database dump
on the VPS) and decide whether the old code can run against the
migrated schema. Prove rollback works once on a quiet day, not first under
pressure.

## Other commands

`list` shows projects and their latest result, `add`/`remove` register
projects, `init` scaffolds a `.cicd.yaml`, `ui` opens the dashboard (needs a
terminal), `version` prints the version, `guide` prints this text.

## Exit status

0 ok; 1 pipeline failed or cancelled; 2 usage error; 3 config could not be
loaded. With a pipe (`| tail`) `$?` is the pipe's; use `set -o pipefail` or
`--json` (its `exit_code` is the status j-cicd returned).
