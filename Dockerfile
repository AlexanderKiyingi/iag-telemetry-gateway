# syntax=docker/dockerfile:1.7
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN set -eu; \
    mkdir -p /out; \
    for cmd in ./cmd/gateway ./cmd/ingest; do \
        name=$(basename $cmd); \
        CGO_ENABLED=0 GOOS=linux go build -trimpath \
            -ldflags="-s -w -X main.version=${VERSION}" \
            -o "/out/$name" "$cmd"; \
    done

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/ /app/
ENV ADDR=:4080 LOG_FORMAT=json
EXPOSE 4080 5027
# Default: HTTP ingest. Override for TCP: command: ["/app/gateway"]
ENTRYPOINT ["/app/ingest"]
