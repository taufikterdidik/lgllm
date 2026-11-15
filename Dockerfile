# Multi-stage build: the final image contains only the static binary.
# CGO is disabled, so the result is a scratch-compatible executable.

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

# ca-certificates and tzdata are copied into the scratch image below.
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /src

# Dependency layer: cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags "-s -w \
        -X lgllm/internal/version.Version=${VERSION} \
        -X lgllm/internal/version.Commit=${COMMIT} \
        -X lgllm/internal/version.Date=${DATE}" \
      -o /out/lgllm ./cmd/lgllm

FROM scratch

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

LABEL org.opencontainers.image.title="lgllm" \
      org.opencontainers.image.description="Lightweight OpenAI/Anthropic-compatible LLM proxy" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}" \
      org.opencontainers.image.source="https://github.com/taufikterdidik/lgllm" \
      org.opencontainers.image.licenses="MIT"

# Binary is static; the only runtime needs are a CA bundle for outbound TLS and
# a timezone database for readable logs.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /out/lgllm /lgllm
COPY --from=build /src/LICENSE /LICENSE

# Run unprivileged. 65532 is the distroless "nonroot" uid, chosen so the same id
# works if this image is ever swapped for a distroless base.
USER 65532:65532

ENV LGLLM_LOG_FORMAT=json

EXPOSE 20128

# The binary itself is the probe, so this works in a scratch image without a
# shell or curl.
HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
  CMD ["/lgllm", "-config", "/etc/lgllm/config.yaml", "-healthcheck"]

ENTRYPOINT ["/lgllm"]
# Listen on the container interface; the operator still controls exposure via
# the published port and auth.mode.
CMD ["-config", "/etc/lgllm/config.yaml", "-listen", "0.0.0.0:20128"]
