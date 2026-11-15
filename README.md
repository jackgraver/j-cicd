# j-cicd

A self-hosted CI/CD pipeline tool written in Go. Single binary, no heavyweight CI server required.

**Features:**
- Portable: runs on any machine (VPS, homelab, laptop, etc.)
- Pipeline: clone/build/test/package/push workflow
- Project-agnostic: driven by `.cicd.yaml` config in your project
- Docker integration: builds and pushes container images
- Central manager: register projects once, run them by name from anywhere,
  with recorded history (status, revision, duration, full logs) and an
  interactive dashboard (`j-cicd`)

```sh
j-cicd add ~/code/my-app      # register (needs a .cicd.yaml; see j-cicd init)
j-cicd run my-app             # run it from anywhere
j-cicd history                # what ran, when, and how it went
j-cicd                        # dashboard: pick a project, run, watch logs
```

**Status:** CI/CD runner and multi-project manager work end-to-end. VPS deploy agent and failure investigation coming soon.

For more details, see [CLAUDE.md](CLAUDE.md). For running it against a real
project, see [DEPLOY.md](DEPLOY.md); for the planned VPS deploy agent, see
[AGENT.md](AGENT.md).
