FROM golang:1.27.2-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/leafr-timelapse-generator .

FROM debian:13.7-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/leafr-timelapse-generator /usr/local/bin/leafr-timelapse-generator
ENV LISTEN_ADDR=:8090 MEDIA_DIR=/media
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=3s CMD ["/usr/local/bin/leafr-timelapse-generator", "-healthcheck"]
ENTRYPOINT ["/usr/local/bin/leafr-timelapse-generator"]
