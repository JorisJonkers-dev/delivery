# syntax=docker/dockerfile:1.10
# The backup method images the Platform document's engines name, one target each: file-backup,
# postgres-backup and rabbitmq-backup. All three run cmd/backup; only what each needs beside it
# differs. Cross-compiles on the build platform, as the Dockerfile beside it does.

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/app ./cmd/backup

# Reads the volume at /data alone.
FROM gcr.io/distroless/static-debian12:nonroot AS file-backup
COPY --from=build /out/app /app
# By number: the images lock refuses a named user. The CronJob runs it as the plan's uid and gid.
USER 65532:65532
ENTRYPOINT ["/app", "files"]

# Reads the broker's definitions over the management API.
FROM gcr.io/distroless/static-debian12:nonroot AS rabbitmq-backup
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app", "rabbitmq"]

# pg_dumpall must be at least the server's major version, which is 17: it refuses a newer server
# outright. Move this with the server.
FROM postgres:17-alpine AS postgres-backup
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app", "postgres"]
