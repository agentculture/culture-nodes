# syntax=docker/dockerfile:1

# Culture Nodes control-plane image.
#
# Multi-stage build: compile the `nodes` binary (cmd/nodes) with the Go
# toolchain, then ship it alone on a distroless nonroot base. No shell, no
# package manager, no Docker socket in the runtime image — code executes only
# through the headspace-cli runner boundary, never from inside this
# container (see CLAUDE.md's "Runtime" ground rule).

FROM golang:1.26 AS build

ARG VERSION=dev

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
# migrations and schemas are go:embed'd by internal/store/postgres and
# internal/contracts respectively (the binary carries its own migrations
# and JSON Schema definitions) -- without them the build fails with
# "no required module provides package .../migrations|schemas".
COPY migrations ./migrations
COPY schemas ./schemas

RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/nodes \
    ./cmd/nodes

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/nodes /nodes

USER nonroot

ENTRYPOINT ["/nodes"]
