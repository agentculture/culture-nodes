# syntax=docker/dockerfile:1

# Culture Nodes control-plane image.
#
# Three stages: build the web SPA (Vite), compile the `nodes` binary with
# the SPA embedded (-tags embedweb, see webassets_embed.go), then ship the
# binary alone on a distroless nonroot base. No shell, no package manager,
# no Docker socket in the runtime image — code executes only through the
# external runner boundary (Lambda, or a host-run headspace bridge), never
# from inside this container.

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

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/nodes /nodes

USER nonroot

ENTRYPOINT ["/nodes"]
