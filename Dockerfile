# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# Static, reproducible binary: no cgo, stripped, trimmed paths.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/llmgate ./cmd/llmgate

# ---- runtime ----
# Distroless static: no shell, no package manager, runs as a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/llmgate /llmgate
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/llmgate"]
CMD ["-config", "/etc/llmgate/config.yaml"]
