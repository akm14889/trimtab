FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/traffic-gen .

FROM alpine:3.21
RUN adduser -S trafficgen
USER trafficgen
COPY --from=builder /out/traffic-gen /usr/local/bin/traffic-gen
ENTRYPOINT ["/usr/local/bin/traffic-gen"]
