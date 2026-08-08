# deploy/compose

Placeholder for the Docker Compose deployment profile (PRD §19). Nothing
lives here yet.

## Images

The control-plane binary (`cmd/nodes`) is published as a multi-arch
(linux/amd64, linux/arm64) OCI image to `ghcr.io/agentculture/culture-nodes`
by the `release.yml` workflow on every `v*` tag, alongside the existing PyPI
lane for the Python mesh-agent surface. Images are pushed **by digest** —
the `<tag>` and `latest` tags both point at that digest, and the digest
itself (not a mutable tag) is what any compose profile added here should
pin, e.g. `ghcr.io/agentculture/culture-nodes@sha256:<digest>`.
