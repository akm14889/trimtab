FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/noisy-app .

FROM alpine:3.21
RUN adduser -S noisy
USER noisy
COPY --from=builder /out/noisy-app /usr/local/bin/noisy-app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/noisy-app"]
