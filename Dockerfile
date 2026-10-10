# syntax=docker/dockerfile:1

# Go version: keep in sync with the `go` directive in go.mod.
FROM golang:1.26.8-alpine AS build
WORKDIR /src

# Dependencies first, so source edits do not invalidate the module cache layer.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .
# CGO off: fully static binaries for a distroless/static runtime.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/wallet ./cmd/wallet \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/healthcheck ./cmd/healthcheck

# No shell, no package manager, non-root user (uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/wallet /out/healthcheck /usr/local/bin/
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=5s --timeout=4s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/healthcheck", "/health/ready"]
ENTRYPOINT ["/usr/local/bin/wallet"]
