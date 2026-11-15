# j-cicd

A self-hosted CI/CD pipeline tool written in Go. Single binary, no heavyweight CI server required.

**Features:**
- Portable: runs on any machine (VPS, homelab, laptop, etc.)
- Pipeline: clone/build/test/package/push workflow
- Project-agnostic: driven by `.cicd.yaml` config in your project
- Docker integration: builds and pushes container images

**Status:** Basic CI/CD runner works end-to-end. VPS deploy agent and failure investigation coming soon.

For more details, see [CLAUDE.md](CLAUDE.md).
