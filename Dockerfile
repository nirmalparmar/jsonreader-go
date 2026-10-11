# syntax=docker/dockerfile:1.7
#
# Targets:
#   runtime     (default) the API server + nsjail + toolchains (+ Chromium with WITH_PDF=true)
#   toolchains  language runtimes only — use as SANDBOX_DOCKER_IMAGE when the
#               server runs on the host with SANDBOX=docker
#
#   docker build -t common-service .
#   docker build --target toolchains -t common-service-runner .

ARG GO_VERSION=1.24
# Languages baked into the image (comma-separated subset), e.g.
#   docker build --build-arg LANGS=python,javascript,c,cpp,java -t common-service .
ARG LANGS=python,javascript,typescript,c,cpp,go,java,rust,ruby,php,bash

# ---- build the server -------------------------------------------------------
FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- build nsjail -----------------------------------------------------------
FROM debian:bookworm-slim AS nsjail
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git make g++ pkg-config flex bison \
      libprotobuf-dev protobuf-compiler libnl-route-3-dev \
    && rm -rf /var/lib/apt/lists/*
ARG NSJAIL_REF=3.4
RUN git clone --depth 1 --branch ${NSJAIL_REF} https://github.com/google/nsjail /nsjail \
    && make -C /nsjail -j"$(nproc)" \
    && strip /nsjail/nsjail

FROM node:22-bookworm-slim AS node

# Minimal Java 21 runtime: just the modules the source launcher and typical
# programs need (~65 MB instead of a ~260 MB JDK), with a CDS archive for fast start.
FROM eclipse-temurin:21-jdk AS jdk
RUN jlink \
      --add-modules java.base,jdk.compiler,java.logging,java.xml,java.sql,java.net.http,jdk.unsupported,jdk.zipfs \
      --strip-debug --no-man-pages --no-header-files --compress=zip-6 --generate-cds-archive \
      --output /opt/jre

# ---- assemble the copied runtimes, keeping only the selected languages -----
FROM debian:bookworm-slim AS runtimes
ARG LANGS
COPY --from=node /usr/local/bin/node /stage/usr/local/bin/node
COPY --from=build /usr/local/go /stage/usr/local/go
COPY --from=jdk /opt/jre /stage/opt/runtimes/java
RUN set -eu; \
    has() { case ",$LANGS," in *",$1,"*) return 0;; esac; return 1; }; \
    if ! has javascript && ! has typescript; then rm -rf /stage/usr/local/bin/node; fi; \
    if ! has java; then rm -rf /stage/opt/runtimes/java; fi; \
    if ! has go; then rm -rf /stage/usr/local/go; else \
      G=/stage/usr/local/go; \
      # Only what `go build` of user programs needs: no tests, docs, toolchain sources or profilers.
      rm -rf $G/test $G/api $G/doc $G/misc $G/src/cmd; \
      find $G/src -type d -name testdata -prune -exec rm -rf {} +; \
      find $G/src -name '*_test.go' -delete; \
      for t in $G/pkg/tool/*/*; do \
        case "$(basename "$t")" in compile|link|asm|cgo|buildid|pack) ;; *) rm -f "$t";; esac; \
      done; \
    fi; \
    mkdir -p /stage/usr/local/bin /stage/opt/runtimes

# ---- language toolchains ----------------------------------------------------
FROM debian:bookworm-slim AS toolchains
ARG LANGS
COPY --from=runtimes /stage/ /
COPY deploy/gocache-warm.go /tmp/warm/main.go
RUN set -eu; \
    has() { case ",$LANGS," in *",$1,"*) return 0;; esac; return 1; }; \
    pkgs="ca-certificates"; \
    if has python; then pkgs="$pkgs python3"; fi; \
    if has c || has cpp || has rust; then pkgs="$pkgs gcc libc6-dev"; fi; \
    if has cpp; then pkgs="$pkgs g++"; fi; \
    if has rust; then pkgs="$pkgs rustc"; fi; \
    if has ruby; then pkgs="$pkgs ruby"; fi; \
    if has php; then pkgs="$pkgs php-cli"; fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends $pkgs; \
    rm -rf /var/lib/apt/lists/* /var/cache/apt /var/log/* /usr/share/doc /usr/share/man /usr/share/info /usr/share/lintian; \
    # Sanitizer runtimes (-fsanitize=...) are never used by the sandbox.
    rm -f /usr/lib/*/libasan.so* /usr/lib/*/libhwasan.so* /usr/lib/*/liblsan.so* /usr/lib/*/libtsan.so* /usr/lib/*/libubsan.so*; \
    if [ -x /opt/runtimes/java/bin/java ]; then ln -s /opt/runtimes/java/bin/java /usr/local/bin/java; fi; \
    if [ -x /usr/local/go/bin/go ]; then \
      ln -s /usr/local/go/bin/go /usr/local/bin/go; \
      # Warm Go build cache, copied into each Go compile (same env as the sandbox).
      cd /tmp/warm && GOCACHE=/opt/gocache GOFLAGS=-mod=mod GOPROXY=off GOTOOLCHAIN=local CGO_ENABLED=0 GOTELEMETRY=off \
        go build -trimpath -o /dev/null main.go; \
      chmod -R a+rX /opt/gocache; \
    fi; \
    rm -rf /tmp/warm /root/.cache /root/go

# ---- runtime ----------------------------------------------------------------
FROM toolchains AS runtime
# Markdown -> PDF is parked: Chromium and its fonts (~800 MB) are only
# installed with --build-arg WITH_PDF=true, which also enables the endpoint.
ARG WITH_PDF=false
RUN apt-get update && apt-get install -y --no-install-recommends \
      tini libprotobuf32 libnl-route-3-200 \
    && if [ "$WITH_PDF" = "true" ]; then \
         apt-get install -y --no-install-recommends chromium \
           fonts-noto-core fonts-noto-cjk fonts-noto-color-emoji fonts-noto-mono \
           fonts-liberation2 fonts-dejavu-core fonts-firacode \
         && groupadd -g 10001 chrome && useradd -u 10001 -g chrome -M -d /nonexistent -s /usr/sbin/nologin chrome; \
       fi \
    && rm -rf /var/lib/apt/lists/* /usr/share/doc /usr/share/man
COPY --from=nsjail /nsjail/nsjail /usr/local/bin/nsjail
COPY --from=build /out/server /usr/local/bin/server
COPY deploy/entrypoint.sh /entrypoint.sh
RUN chmod 0755 /entrypoint.sh && mkdir -p /data /var/lib/sandbox && chmod 0711 /var/lib/sandbox

ENV PDF_ENABLED=${WITH_PDF}
ENV APP_ENV=production \
    ADDR=:8080 \
    DATA_DIR=/data \
    SANDBOX=nsjail \
    SANDBOX_WORK_ROOT=/var/lib/sandbox \
    CHROME_PATH=/usr/bin/chromium \
    CHROME_UID=10001

VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=60s CMD ["/usr/local/bin/server", "-healthcheck"]
ENTRYPOINT ["/usr/bin/tini", "--", "/entrypoint.sh"]
CMD ["/usr/local/bin/server"]
