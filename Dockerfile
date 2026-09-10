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
# Default: HTTP ingest. One image, four binaries — pick one per service:
#
#   Teltonika Codec 8/8E TCP :5027   /app/gateway
#   SinoTrack / HQ TCP       :5013   /app/sinotrack
#   HTTP JSON ingest         :4080   /app/ingest      (this default)
#
# CMD, not ENTRYPOINT, and that distinction is the whole point.
#
# With an exec-form ENTRYPOINT and no CMD, Docker APPENDS whatever the caller
# passes rather than replacing it — so `command: ["/app/sinotrack"]` ran
# `/app/ingest /app/sinotrack`: the ingest binary with a stray argument,
# listening on :4080 and never on :5013. It starts cleanly and logs nothing
# alarming, so the failure surfaces as trackers that connect to nothing.
#
# Both fleet-iot-tcp and fleet-iot-sinotrack in deploy/docker-compose.yml were
# running the ingest for this reason. fleet-iot-ingest worked only by accident.
# Railway's startCommand overrides CMD too, so the same trap applied there.
#
# As CMD, `command:` in Compose and `startCommand` on Railway both do what
# every reader already assumes they do.
CMD ["/app/ingest"]
