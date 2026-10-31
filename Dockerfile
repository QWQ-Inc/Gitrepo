# syntax=docker/dockerfile:1
# Cross-compiles on the build platform; the runtime image only needs git.
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/gitrepo ./cmd/gitrepo \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/gr ./cmd/gr

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git git-lfs openssh-client ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --home-dir /data gitrepo \
 && mkdir -p /data && chown gitrepo /data
COPY --from=build /out/gitrepo /out/gr /usr/local/bin/
USER gitrepo
ENV GITREPO_DATA_DIR=/data \
    GITREPO_LISTEN=:8080
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["gitrepo", "healthcheck"]
ENTRYPOINT ["gitrepo"]
CMD ["serve"]
