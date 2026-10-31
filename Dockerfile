# syntax=docker/dockerfile:1
# Cross-compiles on the build platform; the runtime image only needs git.
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/repohub ./cmd/repohub \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/rh ./cmd/rh

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git openssh-client ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --home-dir /data repohub \
 && mkdir -p /data && chown repohub /data
COPY --from=build /out/repohub /out/rh /usr/local/bin/
USER repohub
ENV REPOHUB_DATA_DIR=/data \
    REPOHUB_LISTEN=:8080
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["repohub", "healthcheck"]
ENTRYPOINT ["repohub"]
CMD ["serve"]
