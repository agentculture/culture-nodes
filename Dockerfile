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

# Cache module downloads separately from source changes. go.sum must come
# along with go.mod -- `go mod download` in readonly mode (the default since
# Go 1.16) fails without it, and a missing go.sum here previously made this
# build fail before it ever reached `go build`.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
# schemas/ and migrations/ are go:embed'd by internal/contracts and
# internal/store/postgres respectively (see their embed.go /
# migrations.go doc comments) -- the binary carries its own schemas and
# migrations, so both directories are build inputs, not deploy-time
# assets. Without them `go build ./cmd/nodes` fails to resolve those two
# packages entirely.
COPY schemas ./schemas
COPY migrations ./migrations

RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/nodes \
    ./cmd/nodes

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/nodes /nodes

USER nonroot

ENTRYPOINT ["/nodes"]
