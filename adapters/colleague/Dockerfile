# colleague-bridge reference image.
#
# Ships SEPARATELY from the culture-nodes control-plane image (PRD c24): this
# container is an actor Culture Nodes reaches over the network
# (internal/actors.Client), never a component the control-plane process
# loads in-process or reaches into via a shared socket. See README.md
# "Deployment model" / "Dockerfile" for the full picture.
#
# What this image does NOT do, on purpose:
#   * it does not embed a specific target repo — `repo_allowlist` paths are
#     container-internal; mount or clone the repo(s) this bridge may
#     dispatch into at deployment time (a bind mount or an init container);
#   * it does not configure a real colleague engine — colleague's own
#     engine/model config (a vLLM endpoint, typically) is supplied via
#     `colleague_env` in the bridge config or COLLEAGUE_* env vars at
#     `docker run` time. `COLLEAGUE_ENGINE=mock` is useful for a smoke test,
#     never a production backend.

FROM python:3.12-slim

# pipx installs `colleague` as an isolated application (its own venv, own
# dependency graph) so it can never collide with this bridge's own
# stdlib-only Python environment — the two share a Python interpreter
# version but nothing else. `git` is required: colleague's isolated-worktree
# work path (`--repo` dispatch) shells out to `git` for every work item.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git pipx \
    && rm -rf /var/lib/apt/lists/* \
    && pipx ensurepath

ENV PATH="/root/.local/bin:${PATH}"

# Pin colleague's version explicitly at build time (matches this bridge's
# "Colleague contract pin + upgrade policy" in README.md) — override with
# `--build-arg COLLEAGUE_VERSION=x.y.z` to move the pin deliberately, never
# silently on a rebuild.
ARG COLLEAGUE_VERSION=""
RUN if [ -n "$COLLEAGUE_VERSION" ]; then \
      pipx install "colleague==${COLLEAGUE_VERSION}"; \
    else \
      pipx install colleague; \
    fi

# --- this bridge --------------------------------------------------------
WORKDIR /opt/colleague-bridge
COPY pyproject.toml README.md ./
COPY src ./src

# Stdlib-only runtime deps (see pyproject.toml) — a plain `pip install .` is
# enough, no lockfile/resolver needed for this package specifically.
RUN pip install --no-cache-dir .

# Repos this bridge may dispatch into are mounted here by the deployer; the
# bridge config's `repo_allowlist` should name paths under this prefix.
VOLUME ["/repos"]

# Config is supplied at `docker run` time (a mounted JSON file + env, or env
# alone — see README.md's config reference). No default config ships in the
# image: an unconfigured bridge has an empty repo_allowlist and refuses
# every invocation, which is the safe failure mode, not a broken one.
ENV COLLEAGUE_BRIDGE_HOST=0.0.0.0
ENV COLLEAGUE_BRIDGE_PORT=8085
EXPOSE 8085

ENTRYPOINT ["colleague-bridge"]
