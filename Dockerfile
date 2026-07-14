# syntax=docker/dockerfile:1

# Build on the runner's native architecture while cross-compiling the static Go
# binary for the requested image platform. This keeps multi-platform release
# builds fast and avoids executing an emulated compiler.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The committed React build is already present for go:embed. Generate the
# remaining templ homepage before compiling.
RUN go generate ./...
# VERSION stamps the binary's reported version. Build with
# `--build-arg VERSION=v1.2.3` (release CI passes the tag); it falls back to
# "dev" for a plain local build, since the .git dir is not in the build context.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/jssblck/akari/internal/version.Version=${VERSION}" \
    -o /out/akari-server ./cmd/akari-server

FROM gcr.io/distroless/static-debian12
ARG VERSION=dev
ARG VCS_REF=unknown
LABEL org.opencontainers.image.title="akari-server" \
      org.opencontainers.image.description="Self-hosted server for searchable coding-agent session history" \
      org.opencontainers.image.source="https://github.com/jssblck/akari" \
      org.opencontainers.image.url="https://github.com/jssblck/akari" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}"
COPY --from=build /out/akari-server /akari-server
COPY LICENSE NOTICE /licenses/akari/
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/akari-server"]
