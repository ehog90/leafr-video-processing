FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/leafr-image-capture-convert .

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends fswebcam v4l-utils ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --no-create-home --groups video app \
 && mkdir -p /usr/share/leafr
COPY --from=build /out/leafr-image-capture-convert /usr/local/bin/leafr-image-capture-convert
COPY overlay/leafr_ver3_white.png /usr/share/leafr/logo.png
COPY fonts/manrope/Manrope-SemiBold.ttf /usr/share/leafr/Manrope-SemiBold.ttf
USER app
ENV LISTEN_ADDR=:8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD ["/usr/local/bin/leafr-image-capture-convert", "-healthcheck"]
ENTRYPOINT ["/usr/local/bin/leafr-image-capture-convert"]
