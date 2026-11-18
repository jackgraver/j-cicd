# j-cicd

A self-hosted CI/CD pipeline tool written in Go. One binary on the build host, one small agent on the VPS, no heavyweight CI server required.

**Features:**
- Portable: runs on any machine (VPS, homelab, laptop, etc.)
- Pipeline: clone/build/test/package/push workflow
- Project-agnostic: driven by `.cicd.yaml` config in your project
- Docker integration: builds and pushes container images
- Central manager: register projects once, run them by name from anywhere,
  with recorded history (status, revision, duration, full logs) and an
  interactive dashboard (`j-cicd`)
- Deploy: hands the pushed digests to `j-cicd-agent` on the VPS over SSH, which rolls them out with docker compose, health-checks them, and rolls back on failure
- Binary deploys: projects that ship files (`artifacts:`) get the same treatment — sent by sha256, installed atomically, their systemd unit restarted and watched, rolled back on failure — on a VPS or on this machine (j-cicd installs itself this way)

Install (needs Go and Docker, e.g. `sudo dnf install golang docker-cli`):

```sh
GOBIN=~/.local/bin go install ./cmd/...   # from a checkout; re-run after pulling
```

```sh
j-cicd add ~/code/my-app      # register (needs a .cicd.yaml; see j-cicd init)
j-cicd run my-app             # run it from anywhere
j-cicd history                # what ran, when, and how it went
j-cicd rollback my-app        # revert the last deploy on the VPS (images, not the database)
j-cicd                        # dashboard: pick a project, run, watch logs
```

**Status:** Build, test, push, deploy and the multi-project manager work end to end. Failure investigation coming later.

For more details, see [CLAUDE.md](CLAUDE.md). For running it against a real
project, see [DEPLOY.md](DEPLOY.md); for the VPS deploy agent and its setup,
see [AGENT.md](AGENT.md).
