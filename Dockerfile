# syntax=docker/dockerfile:1.7

# One image, every role (Task 9.1). The api, worker and migrate binaries come
# from the same modules and must agree on the schema, so they ship as one
# artifact with one tag: the migrate that ran is the code that is serving. Each
# container picks its binary with its command, for example ["/api"].
#
#   docker buildx build --platform linux/arm64 --build-arg GIT_SHA=$(git rev-parse HEAD) -t cleargate-core .
#   docker run --rm -e DATABASE_DSN=... cleargate-core /migrate up

# The build stage runs on the builder's own architecture and cross-compiles,
# which Go does natively with CGO off, so no emulator is needed to build for
# Graviton (linux/arm64, cheaper on Fargate) from an x86 runner.
FROM --platform=$BUILDPLATFORM golang:1.26.4-bookworm AS build

ARG TARGETOS=linux
ARG TARGETARCH=arm64
ARG GIT_SHA=dev

WORKDIR /src

# Modules first, so a code change does not download them again.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal

# -trimpath keeps the builder's paths out of the binary; -s -w drop the symbol
# table and DWARF, which a production binary does not need for stack traces.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${GIT_SHA}" \
      -o /out/ ./cmd/api ./cmd/worker ./cmd/migrate

# Distroless static: no shell, no package manager, nothing to exec into, and
# the nonroot user. It carries CA certificates and tzdata, which the binaries
# need for CDP, the Solana RPC and SES over TLS, and for UTC calendar windows.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/api /out/worker /out/migrate /
COPY migrations /migrations

# Amazon RDS signs its server certificates with its own CAs, which are not in
# the public bundle above. DATABASE_DSN names this file with sslrootcert and
# sslmode=verify-full (Task 13.3), so the database's identity is checked, not
# just encrypted to. It is fetched over HTTPS from AWS at build time; the
# bundle is rotated by AWS, so a pinned checksum would break builds for no gain.
ADD --chmod=0444 https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem /etc/ssl/certs/rds-global-bundle.pem

ENV MIGRATIONS_DIR=/migrations

USER nonroot:nonroot

# No HEALTHCHECK: there is no shell or curl to run one, and on ECS the load
# balancer's GET /v1/health decides whether an API task is healthy. No
# ENTRYPOINT either, so a task definition's command names the binary.
CMD ["/api"]
