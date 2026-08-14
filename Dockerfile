ARG NODE_BUILD_RESOURCE_MODE=auto
ARG NODE_BUILD_LOW_MEMORY_KB=2097152
ARG NODE_BUILD_LOW_BUN_CONCURRENT_SCRIPTS=1
ARG NODE_BUILD_LOW_BUN_NETWORK_CONCURRENCY=1
ARG GO_BUILD_RESOURCE_MODE=auto
ARG GO_BUILD_LOW_MEMORY_KB=2097152
ARG GO_BUILD_LOW_MEMORY_LIMIT_MB=768
ARG GO_BUILD_LOW_MAX_PROCS=1
ARG GO_BUILD_LOW_BUILD_PARALLELISM=1
ARG GO_BUILD_LOW_GOGC=50
ARG DEBIAN_MIRROR=http://mirrors.aliyun.com/debian
ARG DEBIAN_SECURITY_MIRROR=http://mirrors.aliyun.com/debian-security
ARG APT_RETRIES=5
ARG APT_TIMEOUT_SECONDS=30

FROM oven/bun:1@sha256:0733e50325078969732ebe3b15ce4c4be5082f18c4ac1a0f0ca4839c2e4e42a7 AS builder
ARG NODE_BUILD_RESOURCE_MODE
ARG NODE_BUILD_LOW_MEMORY_KB
ARG NODE_BUILD_LOW_BUN_CONCURRENT_SCRIPTS
ARG NODE_BUILD_LOW_BUN_NETWORK_CONCURRENCY

WORKDIR /build/web
COPY web/package.json web/bun.lock ./
RUN set -eu; \
    case "$NODE_BUILD_RESOURCE_MODE" in auto|low|normal) ;; *) echo "invalid NODE_BUILD_RESOURCE_MODE=$NODE_BUILD_RESOURCE_MODE (expected auto|low|normal)" >&2; exit 1 ;; esac; \
    case "$NODE_BUILD_LOW_MEMORY_KB" in ''|*[!0-9]*) echo "NODE_BUILD_LOW_MEMORY_KB must be a positive integer" >&2; exit 1 ;; esac; \
    case "$NODE_BUILD_LOW_BUN_CONCURRENT_SCRIPTS" in ''|*[!0-9]*) echo "NODE_BUILD_LOW_BUN_CONCURRENT_SCRIPTS must be a positive integer" >&2; exit 1 ;; esac; \
    case "$NODE_BUILD_LOW_BUN_NETWORK_CONCURRENCY" in ''|*[!0-9]*) echo "NODE_BUILD_LOW_BUN_NETWORK_CONCURRENCY must be a positive integer" >&2; exit 1 ;; esac; \
    [ "$NODE_BUILD_LOW_MEMORY_KB" -gt 0 ] && [ "$NODE_BUILD_LOW_BUN_CONCURRENT_SCRIPTS" -gt 0 ] && [ "$NODE_BUILD_LOW_BUN_NETWORK_CONCURRENCY" -gt 0 ] || { echo "low-resource Bun parameters must be greater than zero" >&2; exit 1; }; \
    available_memory_kb=0; \
    if [ -r /proc/meminfo ]; then available_memory_kb="$(awk '/^MemAvailable:/ { print $2; exit }' /proc/meminfo)"; fi; \
    available_memory_kb="${available_memory_kb:-0}"; \
    case "$available_memory_kb" in ''|*[!0-9]*) available_memory_kb=0 ;; esac; \
    selected_mode="$NODE_BUILD_RESOURCE_MODE"; \
    if [ "$selected_mode" = auto ]; then \
      if [ "$available_memory_kb" -gt 0 ] && [ "$available_memory_kb" -lt "$NODE_BUILD_LOW_MEMORY_KB" ]; then selected_mode=low; else selected_mode=normal; fi; \
    fi; \
    if [ "$selected_mode" = low ]; then \
      echo "[container-build] stage=frontend-install mode=$selected_mode available_memory_kb=$available_memory_kb threshold_kb=$NODE_BUILD_LOW_MEMORY_KB bun_flags=--smol concurrent_scripts=$NODE_BUILD_LOW_BUN_CONCURRENT_SCRIPTS network_concurrency=$NODE_BUILD_LOW_BUN_NETWORK_CONCURRENCY"; \
      bun --smol install --frozen-lockfile --concurrent-scripts="$NODE_BUILD_LOW_BUN_CONCURRENT_SCRIPTS" --network-concurrency="$NODE_BUILD_LOW_BUN_NETWORK_CONCURRENCY"; \
    else \
      echo "[container-build] stage=frontend-install mode=$selected_mode available_memory_kb=$available_memory_kb threshold_kb=$NODE_BUILD_LOW_MEMORY_KB bun_flags=default concurrent_scripts=default network_concurrency=default"; \
      bun install --frozen-lockfile; \
    fi
COPY ./web ./
COPY ./VERSION /build/VERSION
RUN set -eu; \
    case "$NODE_BUILD_RESOURCE_MODE" in auto|low|normal) ;; *) echo "invalid NODE_BUILD_RESOURCE_MODE=$NODE_BUILD_RESOURCE_MODE (expected auto|low|normal)" >&2; exit 1 ;; esac; \
    case "$NODE_BUILD_LOW_MEMORY_KB" in ''|*[!0-9]*) echo "NODE_BUILD_LOW_MEMORY_KB must be a positive integer" >&2; exit 1 ;; esac; \
    case "$NODE_BUILD_LOW_BUN_CONCURRENT_SCRIPTS" in ''|*[!0-9]*) echo "NODE_BUILD_LOW_BUN_CONCURRENT_SCRIPTS must be a positive integer" >&2; exit 1 ;; esac; \
    case "$NODE_BUILD_LOW_BUN_NETWORK_CONCURRENCY" in ''|*[!0-9]*) echo "NODE_BUILD_LOW_BUN_NETWORK_CONCURRENCY must be a positive integer" >&2; exit 1 ;; esac; \
    [ "$NODE_BUILD_LOW_MEMORY_KB" -gt 0 ] && [ "$NODE_BUILD_LOW_BUN_CONCURRENT_SCRIPTS" -gt 0 ] && [ "$NODE_BUILD_LOW_BUN_NETWORK_CONCURRENCY" -gt 0 ] || { echo "low-resource Bun parameters must be greater than zero" >&2; exit 1; }; \
    available_memory_kb=0; \
    if [ -r /proc/meminfo ]; then available_memory_kb="$(awk '/^MemAvailable:/ { print $2; exit }' /proc/meminfo)"; fi; \
    available_memory_kb="${available_memory_kb:-0}"; \
    case "$available_memory_kb" in ''|*[!0-9]*) available_memory_kb=0 ;; esac; \
    selected_mode="$NODE_BUILD_RESOURCE_MODE"; \
    if [ "$selected_mode" = auto ]; then \
      if [ "$available_memory_kb" -gt 0 ] && [ "$available_memory_kb" -lt "$NODE_BUILD_LOW_MEMORY_KB" ]; then selected_mode=low; else selected_mode=normal; fi; \
    fi; \
    if [ "$selected_mode" = low ]; then \
      echo "[container-build] stage=frontend-build mode=$selected_mode available_memory_kb=$available_memory_kb threshold_kb=$NODE_BUILD_LOW_MEMORY_KB bun_flags=--smol"; \
      DISABLE_ESLINT_PLUGIN='true' VITE_REACT_APP_VERSION="$(cat /build/VERSION)" bun --smol run build; \
    else \
      echo "[container-build] stage=frontend-build mode=$selected_mode available_memory_kb=$available_memory_kb threshold_kb=$NODE_BUILD_LOW_MEMORY_KB bun_flags=default"; \
      DISABLE_ESLINT_PLUGIN='true' VITE_REACT_APP_VERSION="$(cat /build/VERSION)" bun run build; \
    fi

FROM golang:1.26.1-alpine@sha256:2389ebfa5b7f43eeafbd6be0c3700cc46690ef842ad962f6c5bd6be49ed82039 AS builder2
ARG GO_BUILD_RESOURCE_MODE
ARG GO_BUILD_LOW_MEMORY_KB
ARG GO_BUILD_LOW_MEMORY_LIMIT_MB
ARG GO_BUILD_LOW_MAX_PROCS
ARG GO_BUILD_LOW_BUILD_PARALLELISM
ARG GO_BUILD_LOW_GOGC
ENV GO111MODULE=on CGO_ENABLED=0 GOWORK=off

ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=$GOPROXY

ARG TARGETOS
ARG TARGETARCH
ENV GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64}
ENV GOEXPERIMENT=greenteagc

WORKDIR /build

ADD go.mod go.sum ./
# relaykit is a local submodule referenced via replace; its go.mod must be
# present for go mod download to resolve the main module graph.
ADD relaykit/go.mod ./relaykit/go.mod
RUN set -eu; \
    case "$GO_BUILD_RESOURCE_MODE" in auto|low|normal) ;; *) echo "invalid GO_BUILD_RESOURCE_MODE=$GO_BUILD_RESOURCE_MODE (expected auto|low|normal)" >&2; exit 1 ;; esac; \
    case "$GO_BUILD_LOW_MEMORY_KB" in ''|*[!0-9]*) echo "GO_BUILD_LOW_MEMORY_KB must be a positive integer" >&2; exit 1 ;; esac; \
    case "$GO_BUILD_LOW_MEMORY_LIMIT_MB" in ''|*[!0-9]*) echo "GO_BUILD_LOW_MEMORY_LIMIT_MB must be a positive integer" >&2; exit 1 ;; esac; \
    case "$GO_BUILD_LOW_MAX_PROCS" in ''|*[!0-9]*) echo "GO_BUILD_LOW_MAX_PROCS must be a positive integer" >&2; exit 1 ;; esac; \
    case "$GO_BUILD_LOW_BUILD_PARALLELISM" in ''|*[!0-9]*) echo "GO_BUILD_LOW_BUILD_PARALLELISM must be a positive integer" >&2; exit 1 ;; esac; \
    case "$GO_BUILD_LOW_GOGC" in ''|*[!0-9]*) echo "GO_BUILD_LOW_GOGC must be a positive integer" >&2; exit 1 ;; esac; \
    [ "$GO_BUILD_LOW_MEMORY_KB" -gt 0 ] && [ "$GO_BUILD_LOW_MEMORY_LIMIT_MB" -gt 0 ] && [ "$GO_BUILD_LOW_MAX_PROCS" -gt 0 ] && [ "$GO_BUILD_LOW_BUILD_PARALLELISM" -gt 0 ] && [ "$GO_BUILD_LOW_GOGC" -gt 0 ] || { echo "low-resource Go parameters must be greater than zero" >&2; exit 1; }; \
    available_memory_kb=0; \
    if [ -r /proc/meminfo ]; then available_memory_kb="$(awk '/^MemAvailable:/ { print $2; exit }' /proc/meminfo)"; fi; \
    available_memory_kb="${available_memory_kb:-0}"; \
    case "$available_memory_kb" in ''|*[!0-9]*) available_memory_kb=0 ;; esac; \
    selected_mode="$GO_BUILD_RESOURCE_MODE"; \
    if [ "$selected_mode" = auto ]; then \
      if [ "$available_memory_kb" -gt 0 ] && [ "$available_memory_kb" -lt "$GO_BUILD_LOW_MEMORY_KB" ]; then selected_mode=low; else selected_mode=normal; fi; \
    fi; \
    if [ "$selected_mode" = low ]; then \
      echo "[container-build] stage=go-mod-download mode=$selected_mode available_memory_kb=$available_memory_kb threshold_kb=$GO_BUILD_LOW_MEMORY_KB gomaxprocs=$GO_BUILD_LOW_MAX_PROCS gomemlimit=${GO_BUILD_LOW_MEMORY_LIMIT_MB}MiB gogc=$GO_BUILD_LOW_GOGC"; \
      GOMAXPROCS="$GO_BUILD_LOW_MAX_PROCS" GOMEMLIMIT="${GO_BUILD_LOW_MEMORY_LIMIT_MB}MiB" GOGC="$GO_BUILD_LOW_GOGC" go mod download; \
    else \
      echo "[container-build] stage=go-mod-download mode=$selected_mode available_memory_kb=$available_memory_kb threshold_kb=$GO_BUILD_LOW_MEMORY_KB gomaxprocs=default gomemlimit=default gogc=default"; \
      go mod download; \
    fi

COPY . .
COPY --from=builder /build/web/dist ./web/dist
RUN set -eu; \
    case "$GO_BUILD_RESOURCE_MODE" in auto|low|normal) ;; *) echo "invalid GO_BUILD_RESOURCE_MODE=$GO_BUILD_RESOURCE_MODE (expected auto|low|normal)" >&2; exit 1 ;; esac; \
    case "$GO_BUILD_LOW_MEMORY_KB" in ''|*[!0-9]*) echo "GO_BUILD_LOW_MEMORY_KB must be a positive integer" >&2; exit 1 ;; esac; \
    case "$GO_BUILD_LOW_MEMORY_LIMIT_MB" in ''|*[!0-9]*) echo "GO_BUILD_LOW_MEMORY_LIMIT_MB must be a positive integer" >&2; exit 1 ;; esac; \
    case "$GO_BUILD_LOW_MAX_PROCS" in ''|*[!0-9]*) echo "GO_BUILD_LOW_MAX_PROCS must be a positive integer" >&2; exit 1 ;; esac; \
    case "$GO_BUILD_LOW_BUILD_PARALLELISM" in ''|*[!0-9]*) echo "GO_BUILD_LOW_BUILD_PARALLELISM must be a positive integer" >&2; exit 1 ;; esac; \
    case "$GO_BUILD_LOW_GOGC" in ''|*[!0-9]*) echo "GO_BUILD_LOW_GOGC must be a positive integer" >&2; exit 1 ;; esac; \
    [ "$GO_BUILD_LOW_MEMORY_KB" -gt 0 ] && [ "$GO_BUILD_LOW_MEMORY_LIMIT_MB" -gt 0 ] && [ "$GO_BUILD_LOW_MAX_PROCS" -gt 0 ] && [ "$GO_BUILD_LOW_BUILD_PARALLELISM" -gt 0 ] && [ "$GO_BUILD_LOW_GOGC" -gt 0 ] || { echo "low-resource Go parameters must be greater than zero" >&2; exit 1; }; \
    available_memory_kb=0; \
    if [ -r /proc/meminfo ]; then available_memory_kb="$(awk '/^MemAvailable:/ { print $2; exit }' /proc/meminfo)"; fi; \
    available_memory_kb="${available_memory_kb:-0}"; \
    case "$available_memory_kb" in ''|*[!0-9]*) available_memory_kb=0 ;; esac; \
    selected_mode="$GO_BUILD_RESOURCE_MODE"; \
    if [ "$selected_mode" = auto ]; then \
      if [ "$available_memory_kb" -gt 0 ] && [ "$available_memory_kb" -lt "$GO_BUILD_LOW_MEMORY_KB" ]; then selected_mode=low; else selected_mode=normal; fi; \
    fi; \
    if [ "$selected_mode" = low ]; then \
      echo "[container-build] stage=go-build mode=$selected_mode available_memory_kb=$available_memory_kb threshold_kb=$GO_BUILD_LOW_MEMORY_KB gomaxprocs=$GO_BUILD_LOW_MAX_PROCS gomemlimit=${GO_BUILD_LOW_MEMORY_LIMIT_MB}MiB gogc=$GO_BUILD_LOW_GOGC build_parallelism=$GO_BUILD_LOW_BUILD_PARALLELISM"; \
      GOMAXPROCS="$GO_BUILD_LOW_MAX_PROCS" GOMEMLIMIT="${GO_BUILD_LOW_MEMORY_LIMIT_MB}MiB" GOGC="$GO_BUILD_LOW_GOGC" go build -p "$GO_BUILD_LOW_BUILD_PARALLELISM" -ldflags "-s -w -X 'github.com/QuantumNous/new-api/common.Version=$(cat VERSION)'" -o new-api; \
    else \
      unset GOMAXPROCS GOMEMLIMIT GOGC; \
      echo "[container-build] stage=go-build mode=$selected_mode available_memory_kb=$available_memory_kb threshold_kb=$GO_BUILD_LOW_MEMORY_KB gomaxprocs=default gomemlimit=default gogc=default build_parallelism=default"; \
      go build -ldflags "-s -w -X 'github.com/QuantumNous/new-api/common.Version=$(cat VERSION)'" -o new-api; \
    fi

FROM debian:bookworm-slim@sha256:f06537653ac770703bc45b4b113475bd402f451e85223f0f2837acbf89ab020a
ARG DEBIAN_MIRROR
ARG DEBIAN_SECURITY_MIRROR
ARG APT_RETRIES
ARG APT_TIMEOUT_SECONDS

RUN set -eu; \
    case "$APT_RETRIES" in ''|*[!0-9]*) echo "APT_RETRIES must be a positive integer" >&2; exit 1 ;; esac; \
    case "$APT_TIMEOUT_SECONDS" in ''|*[!0-9]*) echo "APT_TIMEOUT_SECONDS must be a positive integer" >&2; exit 1 ;; esac; \
    [ "$APT_RETRIES" -gt 0 ] && [ "$APT_TIMEOUT_SECONDS" -gt 0 ] || { echo "APT retry parameters must be greater than zero" >&2; exit 1; }; \
    for source_file in /etc/apt/sources.list /etc/apt/sources.list.d/debian.sources; do \
      if [ -f "$source_file" ]; then \
        sed -i \
          -e "s|http://deb.debian.org/debian-security|$DEBIAN_SECURITY_MIRROR|g" \
          -e "s|https://deb.debian.org/debian-security|$DEBIAN_SECURITY_MIRROR|g" \
          -e "s|http://deb.debian.org/debian|$DEBIAN_MIRROR|g" \
          -e "s|https://deb.debian.org/debian|$DEBIAN_MIRROR|g" \
          "$source_file"; \
      fi; \
    done; \
    printf 'Acquire::Retries "%s";\nAcquire::http::Timeout "%s";\nAcquire::https::Timeout "%s";\n' \
      "$APT_RETRIES" "$APT_TIMEOUT_SECONDS" "$APT_TIMEOUT_SECONDS" \
      > /etc/apt/apt.conf.d/80-openship-retries; \
    echo "[container-build] apt_mirror=$DEBIAN_MIRROR apt_security_mirror=$DEBIAN_SECURITY_MIRROR retries=$APT_RETRIES timeout_seconds=$APT_TIMEOUT_SECONDS"; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates tzdata libasan8 wget gosu; \
    rm -rf /var/lib/apt/lists/*; \
    update-ca-certificates; \
    useradd --system --create-home --home-dir /home/newapi --shell /usr/sbin/nologin newapi; \
    mkdir -p /data /data/logs; \
    chown -R newapi:newapi /data

COPY --from=builder2 /build/new-api /
COPY LICENSE NOTICE THIRD-PARTY-LICENSES.md /licenses/
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 0755 /usr/local/bin/docker-entrypoint.sh
EXPOSE 3000
WORKDIR /data
USER root
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 CMD wget -q -O - http://127.0.0.1:3000/api/status | grep -Eq '"success"[[:space:]]*:[[:space:]]*true'
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["/new-api"]
