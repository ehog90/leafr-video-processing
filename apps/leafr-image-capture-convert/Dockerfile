FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/leafr-image-capture-convert .

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends fswebcam v4l-utils ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --no-create-home --groups video app \
 && mkdir -p /data/images && chown app /data/images
COPY --from=build /out/leafr-image-capture-convert /usr/local/bin/leafr-image-capture-convert
USER app
ENV OUTPUT_DIR=/data/images LISTEN_ADDR=:8080
VOLUME /data/images
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD ["/usr/local/bin/leafr-image-capture-convert", "-healthcheck"]
ENTRYPOINT ["/usr/local/bin/leafr-image-capture-convert"]
