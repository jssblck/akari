# Build the server as a static binary, then run it from a minimal image.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# VERSION stamps the binary's reported version. Build with
# `--build-arg VERSION=v1.2.3` (release CI passes the tag); it falls back to
# "dev" for a plain local build, since the .git dir is not in the build context.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X github.com/jssblck/akari/internal/version.Version=${VERSION}" \
    -o /out/akari-server ./cmd/akari-server

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/akari-server /akari-server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/akari-server"]
