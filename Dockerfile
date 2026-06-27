# Build the server as a static binary, then run it from a minimal image.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/akari-server ./cmd/akari-server

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/akari-server /akari-server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/akari-server"]
