# syntax=docker/dockerfile:1
# tasmofleet container image.
#  - build:   static Go binary (CGO off, pure-Go SQLite)
#  - decoder: decode-config (LGPL-3.0, github.com/tasmota/decode-config), the
#             upstream PyInstaller single-file build, pinned by version and
#             SHA-256. It is executed as a separate program, never linked.
#             Build with --build-arg WITH_DECODER=false to leave it out.
#  - final:   distroless cc (glibc, needed by the PyInstaller build), nonroot.

ARG GO_VERSION=1.27
ARG DECODER_VERSION=v16.0.0
ARG DECODER_SHA256=00f6fde13ddae11b497252e7ddc421b35c009d3bc795cde219e0dd475a410622

FROM docker.io/library/golang:${GO_VERSION}-trixie AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY changelog.go CHANGELOG.md ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/tasmofleet ./cmd/tasmofleet \
 && mkdir -p /out/data/tmp

FROM docker.io/library/debian:trixie-slim AS decoder
ARG DECODER_VERSION
ARG DECODER_SHA256
ARG WITH_DECODER=true
ADD https://github.com/tasmota/decode-config/releases/download/${DECODER_VERSION}/decode-config-ubuntu-22.04.tar.gz /tmp/dc.tgz
# The upstream build exists for x86_64 only and needs libz from the system.
RUN mkdir -p /out/bin /out/lib && if [ "$WITH_DECODER" = "true" ]; then \
      echo "${DECODER_SHA256}  /tmp/dc.tgz" | sha256sum -c - \
      && tar -xzf /tmp/dc.tgz -C /out/bin decode-config \
      && chmod 0755 /out/bin/decode-config \
      && cp -L /lib/x86_64-linux-gnu/libz.so.1 /out/lib/; \
    fi

FROM gcr.io/distroless/cc-debian13:nonroot
COPY --from=build /out/tasmofleet /usr/local/bin/tasmofleet
COPY --from=decoder /out/bin/ /usr/local/bin/
COPY --from=decoder /out/lib/ /usr/lib/x86_64-linux-gnu/
COPY --from=build --chown=65532:65532 /out/data /data
ENV TASMOFLEET_DATA_DIR=/data \
    TMPDIR=/data/tmp
VOLUME ["/data"]
# 8080: UI/API (behind the reverse proxy). 80: firmware for devices (plain HTTP).
EXPOSE 8080 80
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/tasmofleet", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/tasmofleet"]
CMD ["serve"]
