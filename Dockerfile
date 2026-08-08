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
COPY go.mod ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/nodes \
    ./cmd/nodes

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/nodes /nodes

USER nonroot

ENTRYPOINT ["/nodes"]
