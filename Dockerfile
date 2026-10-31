# syntax=docker/dockerfile:1
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/repohub ./cmd/repohub \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/rh ./cmd/rh

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
ENTRYPOINT ["repohub"]
CMD ["serve"]
