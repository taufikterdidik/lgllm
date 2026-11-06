# Multi-stage build: the final image contains only the static binary.
# CGO is disabled, so the result is a scratch-compatible executable.

FROM golang:1.24-alpine AS build

# ca-certificates and tzdata are copied into the scratch image below.
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /src

# Dependency layer: cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w \
        -X lgllm/internal/version.Version=${VERSION} \
        -X lgllm/internal/version.Commit=${COMMIT} \
        -X lgllm/internal/version.Date=${DATE}" \
      -o /out/lgllm ./cmd/lgllm

FROM scratch

# Binary is static; the only runtime needs are a CA bundle for outbound TLS and
# a timezone database for readable logs.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /out/lgllm /lgllm

# Run unprivileged. 65532 is the distroless "nonroot" uid, chosen so the same id
# works if this image is ever swapped for a distroless base.
USER 65532:65532

ENV LGLLM_LOG_FORMAT=json

EXPOSE 20128

ENTRYPOINT ["/lgllm"]
CMD ["-config", "/etc/lgllm/config.yaml"]