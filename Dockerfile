# syntax=docker/dockerfile:1.7
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN set -eu; \
    mkdir -p /out; \
    for cmd in ./cmd/gateway ./cmd/ingest ./cmd/sinotrack; do \
        name=$(basename $cmd); \
        CGO_ENABLED=0 GOOS=linux go build -trimpath \
            -ldflags="-s -w -X main.version=${VERSION}" \
            -o "/out/$name" "$cmd"; \
    done

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/ /app/
ENV ADDR=:4080 LOG_FORMAT=json
# 4080 HTTP ingest · 5027 Teltonika Codec 8 TCP · 5013 SinoTrack/HQ TCP (ST-901/906/915)
EXPOSE 4080 5027 5013
# Default: HTTP ingest. Override the entrypoint per protocol:
#   Teltonika TCP gateway:  command: ["/app/gateway"]
#   SinoTrack/HQ TCP gateway: command: ["/app/sinotrack"]
ENTRYPOINT ["/app/ingest"]
