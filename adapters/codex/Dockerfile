# codex-bridge reference image.
#
# Ships SEPARATELY from the culture-nodes control-plane image, and
# separately from adapters/colleague's own image (PRD c24): this container
# is an actor Culture Nodes reaches over the network (internal/actors.Client),
# never a component the control-plane process loads in-process or reaches
# into via a shared socket. See README.md "Deployment model" for the full
# picture.
#
# What this image does NOT do, on purpose:
#   * it does not embed a specific target repo — `repo_allowlist` paths are
#     container-internal; mount or clone the repo(s) this bridge may
#     dispatch into at deployment time (a bind mount or an init container);
#   * it does not authenticate codex — `codex login` state (ChatGPT session
#     or API key, under $CODEX_HOME) is supplied at `docker run` time (a
#     mounted $CODEX_HOME, or CODEX_HOME env pointing at one) — this image
#     never bakes in credentials.

FROM node:20-slim

# codex-cli ships as an npm package (@openai/codex). `git` is required:
# codex exec refuses to run outside a git repo (this bridge never passes
# --skip-git-repo-check), and its own file-edit tools shell out to it.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git python3 \
    && rm -rf /var/lib/apt/lists/*

# Pin codex's version explicitly at build time (matches adapters/colleague's
# own "contract pin + upgrade policy" stance) — override with
# `--build-arg CODEX_VERSION=x.y.z` to move the pin deliberately, never
# silently on a rebuild.
ARG CODEX_VERSION="0.144.6"
RUN npm install -g "@openai/codex@${CODEX_VERSION}"

# --- this bridge --------------------------------------------------------
WORKDIR /opt/codex-bridge
COPY pyproject.toml README.md ./
COPY src ./src

# Stdlib-only runtime deps (see pyproject.toml) — a plain `pip install .` is
# enough, no lockfile/resolver needed for this package specifically.
RUN python3 -m pip install --no-cache-dir --break-system-packages .

# Repos this bridge may dispatch into are mounted here by the deployer; the
# bridge config's `repo_allowlist` should name paths under this prefix.
VOLUME ["/repos"]

# Config is supplied at `docker run` time (a mounted JSON file + env, or env
# alone — see README.md's config reference). No default config ships in the
# image: an unconfigured bridge has an empty repo_allowlist and refuses
# every invocation, which is the safe failure mode, not a broken one.
ENV CODEX_BRIDGE_HOST=0.0.0.0
ENV CODEX_BRIDGE_PORT=8086
EXPOSE 8086

ENTRYPOINT ["codex-bridge"]
