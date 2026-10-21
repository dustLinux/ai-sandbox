# syntax=docker/dockerfile:1

# ---- build -----------------------------------------------------------------
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/sandbox-pool ./cmd/sandbox-pool && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/sandboxd ./cmd/sandboxd

# ---- runtime ---------------------------------------------------------------
# The image root is read only at runtime: state lives in the /data volume,
# the binaries live in the image.
FROM alpine:3.20
RUN apk add --no-cache rclone openssh-client ca-certificates bash coreutils tzdata
WORKDIR /app
COPY --from=build /out/sandbox-pool /app/bin/sandbox-pool
COPY --from=build /out/sandboxd /app/bin/sandboxd
COPY deploy/start.sh /app/start.sh
RUN chmod 0755 /app/start.sh /app/bin/sandbox-pool /app/bin/sandboxd

ENV POOL_STATE_DIR=/data/pool \
    POOL_BIN_DIR=/app/bin \
    RCLONE_CONFIG=/data/rclone/rclone.conf

EXPOSE 3000
CMD ["/app/start.sh"]
