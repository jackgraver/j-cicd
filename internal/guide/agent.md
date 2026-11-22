# j-cicd guide for AI agents

j-cicd builds, tests, pushes and deploys registered projects. This guide is
compiled into the binary you are running, so it matches this version. Prefer
asking the tool over trusting remembered details: `j-cicd run --plan`,
`j-cicd <command> -h`, `j-cicd logs`.

## Rules

- Deploy through j-cicd only. Never build images by hand or ssh to the VPS to
  deploy.
- Ask the user before a production deploy (`run` that pushes and deploys)
  unless they already told you to deploy.
- A run builds the committed release branch (or the working tree if the
  project sets no `branch:`). Commit first; a dirty tree can't be deployed.

## Deploy workflow

1. `j-cicd run <name|dir> --plan` shows the commit, steps, images, deploy
   host and verify URLs without doing anything. Exit 1 means a PROBLEM
   would refuse the run. `--plan --json` is machine-readable.
2. `j-cicd run <name|dir>` runs the pipeline. Without a terminal it prints
   one line per stage and the failing stage's last lines. `-v` streams
   everything, `--quiet` prints only the final status, `--json` prints one
   result object on stdout.
3. `--no-push` builds only; `--no-deploy` pushes images without deploying.
   `--deploy-dirty` overrides the dirty-tree refusal; don't use it unasked.

## When a run fails

- `j-cicd logs <name|run-id> --tail 60` for the failing output; `history`
  lists recorded runs; `cancel` stops a run in progress.
- A failure at the verify stage means the release IS running but the public
  site isn't serving it (yet). Investigate before rolling back.
- Failed deploys roll back by themselves on the VPS; verify failures never do.

## Rollback

`j-cicd rollback <name>` reverts the last deploy: images or files, never the
database. It never happens automatically. Running it again undoes it.

## Other commands

`list` shows projects and their latest result, `add`/`remove` register
projects, `init` scaffolds a `.cicd.yaml`, `ui` opens the dashboard (needs a
terminal), `version` prints the version, `guide` prints this text.

## Exit status

0 ok; 1 pipeline failed or cancelled; 2 usage error; 3 config could not be
loaded. With a pipe (`| tail`) `$?` is the pipe's; use `set -o pipefail` or
`--json`.
