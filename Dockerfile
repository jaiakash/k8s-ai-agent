# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# graph. client-go is large; this matters.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/kai-mcp-server ./cmd/kai-mcp-server

# Distroless static: the binary is CGO-free and speaks only HTTPS to the API
# server, so there is nothing else to ship. No shell is also a real mitigation
# here — the whole point of this rewrite was to stop executing commands.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/kai-mcp-server /usr/local/bin/kai-mcp-server

USER nonroot:nonroot
EXPOSE 8080

# Read-only over Streamable HTTP. Grant writes deliberately with
# --allow-writes, or KAI_ALLOW_WRITES=true.
ENTRYPOINT ["/usr/local/bin/kai-mcp-server"]
CMD ["--transport", "http", "--addr", ":8080"]
