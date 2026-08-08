# deploy/helm

Placeholder for the Helm chart deployment profile (PRD §19). Nothing lives
here yet.

## Images

The control-plane binary (`cmd/nodes`) is published as a multi-arch
(linux/amd64, linux/arm64) OCI image to `ghcr.io/agentculture/culture-nodes`
by the `release.yml` workflow on every `v*` tag, alongside the existing PyPI
lane for the Python mesh-agent surface. Images are pushed **by digest** —
the `<tag>` and `latest` tags both point at that digest. The chart added
here (t22) should pin `image.digest` (`sha256:<digest>`) in its values, not
a mutable tag, so every role Deployment (api/scheduler/worker) sharing the
one image runs the exact artifact `release.yml`'s smoke job already
verified.
