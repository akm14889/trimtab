FROM golang:1.27-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/trimtab ./cmd/trimtab

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S trimtab \
    && adduser -S -G trimtab trimtab

WORKDIR /workspace
COPY --from=builder /out/trimtab /usr/local/bin/trimtab
COPY --chown=trimtab:trimtab examples/demo /workspace/examples/demo

USER trimtab
ENTRYPOINT ["/usr/local/bin/trimtab"]
CMD ["--help"]
