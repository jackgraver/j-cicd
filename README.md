# j-cicd

A self-hosted CI/CD pipeline tool written in Go. One binary on the build host, one small agent on the VPS, no heavyweight CI server required.

**Features:**
- Portable: runs on any machine (VPS, homelab, laptop, etc.)
- Pipeline: clone/build/test/package/push workflow
- Project-agnostic: driven by `.cicd.yaml` config in your project
- Docker integration: builds and pushes container images
- Deploy: hands the pushed digests to `j-cicd-agent` on the VPS over SSH, which rolls them out with docker compose, health-checks them, and rolls back on failure

**Status:** Build, test, push and deploy work end to end. Failure investigation coming later.

For more details, see [CLAUDE.md](CLAUDE.md). For running it against a real
project, see [DEPLOY.md](DEPLOY.md); for the VPS deploy agent and its setup,
see [AGENT.md](AGENT.md).
