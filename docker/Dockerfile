ARG STICKY_OVERSEER_REPO=https://github.com/whisper-darkly/sticky-overseer
ARG STICKY_OVERSEER_REF=

FROM golang:1.24-alpine AS builder

ARG STICKY_OVERSEER_REPO
ARG STICKY_OVERSEER_REF

RUN apk add --no-cache git make
RUN go install github.com/swaggo/swag/cmd/swag@latest

# Build sticky-recorder from the repo root (mounted as build context)
WORKDIR /src/sticky-recorder
COPY . ./
RUN make install PREFIX=/out

# Clone and build sticky-overseer
WORKDIR /src
RUN if [ -n "${STICKY_OVERSEER_REF}" ]; then \
        git clone --depth=1 --branch "${STICKY_OVERSEER_REF}" "${STICKY_OVERSEER_REPO}" sticky-overseer; \
    else \
        git clone --depth=1 "${STICKY_OVERSEER_REPO}" sticky-overseer; \
    fi
WORKDIR /src/sticky-overseer
RUN make install PREFIX=/out


FROM alpine:3.21

RUN apk add --no-cache ffmpeg

COPY --from=builder /out/bin/ /usr/local/bin/
COPY --from=builder /src/sticky-recorder/docker/config.yaml /etc/sticky-overseer/config.yaml

# sticky-overseer
ENV OVERSEER_CONFIG=/etc/sticky-overseer/config.yaml
ENV OVERSEER_PORT=8080
ENV OVERSEER_TRUSTED_CIDRS=""
ENV OVERSEER_LOG_FILE=/data/overseer.jsonl
ENV OVERSEER_DB=/data/overseer.db

# sticky-recorder
ENV STICKY_OUT=/data/recordings/{{.Driver}}/{{.Source}}/{{.Session.Year}}-{{.Session.Month}}-{{.Session.Day}}_{{.Session.Hour}}-{{.Session.Minute}}-{{.Session.Second}}/{{.Recording.Year}}-{{.Recording.Month}}-{{.Recording.Day}}_{{.Recording.Hour}}-{{.Recording.Minute}}-{{.Recording.Second}}
ENV STICKY_DRIVER=chaturbate
ENV STICKY_RESOLUTION=720
ENV STICKY_FRAMERATE=30
ENV STICKY_SEGMENT_LENGTH=""
ENV STICKY_COOKIES=""
ENV STICKY_USER_AGENT=""
ENV STICKY_LOG=""
ENV STICKY_LOG_LEVEL=error
ENV STICKY_OUTPUT_FORMAT=json
ENV STICKY_RETRY_DELAY=5s
ENV STICKY_SEGMENT_TIMEOUT=5m
ENV STICKY_RECORDING_TIMEOUT=30m
ENV STICKY_CHECK_INTERVAL=1m
ENV STICKY_EXEC_FATAL=""
ENV STICKY_EXTERNAL_COOKIES=""
ENV STICKY_COOKIES_SAFE_DOMAINS=""
ENV STICKY_COOKIES_JSON=""
ENV STICKY_COOKIES_REFRESH=""

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://localhost:${OVERSEER_PORT}/ws 2>&1 | grep -q "400 Bad Request" || exit 1

CMD ["sticky-overseer"]
