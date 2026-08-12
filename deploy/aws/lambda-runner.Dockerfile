# syntax=docker/dockerfile:1

# The culture-nodes Lambda runner image: cmd/nodes-runner-lambda compiled
# static, on an Alpine base.
#
# Why Alpine and not distroless: the runner's entire job is executing an
# operation's argv inside this image, so the image must actually contain
# a userland for that argv to name (`true`, `sh -c`-free coreutils, git can
# be layered later). Distroless would leave nothing to execute. The runtime
# loop itself needs no libc — CGO is off and the binary is static.
#
# Build from the repo root (the Go module is the whole repo):
#
#   docker build -f deploy/aws/lambda-runner.Dockerfile \
#     -t <account>.dkr.ecr.<region>.amazonaws.com/culture-nodes/runner:<tag> .
#
# Architecture follows the build host (arm64 on spark); the Lambda function
# must be created with the matching --architectures value.

FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
    -o /nodes-runner-lambda ./cmd/nodes-runner-lambda

FROM alpine:3.22

# Non-root by default (PRD §16.4's runner isolation posture starts here;
# Execution.User is a per-operation refinement this minimal runner does not
# yet honour).
RUN adduser -D -u 10001 runner
COPY --from=build /nodes-runner-lambda /usr/local/bin/nodes-runner-lambda
USER runner

ENTRYPOINT ["/usr/local/bin/nodes-runner-lambda"]
