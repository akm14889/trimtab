FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=local

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/trimtab ./cmd/trimtab

FROM alpine:3.21

LABEL org.opencontainers.image.title="Trimtab" \
      org.opencontainers.image.description="Vendor-neutral adaptive telemetry control plane" \
      org.opencontainers.image.licenses="Apache-2.0"

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S trimtab \
    && adduser -S -G trimtab trimtab

WORKDIR /workspace
COPY --from=builder /out/trimtab /usr/local/bin/trimtab
COPY --chown=trimtab:trimtab examples/demo /workspace/examples/demo

USER trimtab
ENTRYPOINT ["/usr/local/bin/trimtab"]
CMD ["--help"]
