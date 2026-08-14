# syntax=docker/dockerfile:1

# Culture Nodes control-plane image.
#
# Three stages: build the web SPA (Vite), compile the `nodes` binary with
# the SPA embedded (-tags embedweb, see webassets_embed.go), then ship the
# binary alone on a distroless nonroot base. No shell, no package manager,
# no Docker socket in the runtime image — code executes only through the
# external runner boundary (Lambda, or a host-run headspace bridge), never
# from inside this container.
#
# The build stage also compiles cmd/nodes-notifier (economy-discord-graphs
# task t14) into the same image, and the final stage ships both binaries.
# This is deploy/prod's smallest honest option for giving the notifier a
# deployable artifact: nodes-notifier needs no web assets, no schemas, and
# no migrations (it only ever issues GET requests against the control
# plane's own public HTTP surface — see internal/notifier/doc.go), so
# reusing the already-checked-out source and the already-running Go build
# stage costs one extra `go build` rather than a second Dockerfile, a
# second build context, or a second image to ship/tag/build natively on
# each aarch64 production host. The default ENTRYPOINT stays `/nodes`
# unchanged — every existing compose service (migrate/api/scheduler/worker)
# is unaffected — and deploy/prod/compose.thor.yml's new `notifier` service
# selects the second binary with an `entrypoint:` override.

FROM node:24-slim AS webbuild

WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web ./
RUN npm run build

FROM golang:1.26 AS build

ARG VERSION=dev

WORKDIR /src

# Cache module downloads separately from source changes. go.sum must come
# along with go.mod -- `go mod download` in readonly mode fails without it.
COPY go.mod go.sum ./
RUN go mod download

COPY webassets_stub.go webassets_embed.go ./
COPY cmd ./cmd
COPY internal ./internal
# migrations and schemas are go:embed'd by internal/store/postgres and
# internal/contracts respectively — without them the build fails.
COPY schemas ./schemas
COPY migrations ./migrations
COPY --from=webbuild /src/web/dist ./web/dist

RUN CGO_ENABLED=0 go build \
    -trimpath \
    -tags embedweb \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/nodes \
    ./cmd/nodes

RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/nodes-notifier \
    ./cmd/nodes-notifier

# Empty directory the final stage copies in as the notifier's cursor mount
# point (see its COPY --chown below for why it must pre-exist).
RUN mkdir -p /out/notifier-state

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/nodes /nodes
COPY --from=build /out/nodes-notifier /nodes-notifier

# The notifier's cursor directory must exist in the image, owned by nonroot,
# BEFORE a named volume is mounted over it: Docker seeds an empty named
# volume from the image path it covers, ownership included, and a volume
# created against a missing path is root-owned instead. Found live on thor —
# the daemon persists its cursor before delivering (its exactly-once
# guarantee), so a read-only cursor directory blocked delivery entirely
# rather than risking duplicate posts.
COPY --from=build --chown=nonroot:nonroot /out/notifier-state /var/lib/nodes-notifier

USER nonroot

ENTRYPOINT ["/nodes"]
