# WaGo for a Linux server. Builds on the target itself (no cross-compiling cgo): docker build -t wago .
FROM golang:1.27-alpine AS build
RUN apk add --no-cache build-base
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -tags sqlite_fts5 -o /wago .

FROM alpine:3.22
# ffmpeg: voice notes and video previews; tzdata: log times
RUN apk add --no-cache ca-certificates ffmpeg tzdata && adduser -D -H -u 10001 wago && mkdir /data && chown wago /data
COPY --from=build /wago /usr/local/bin/wago
USER wago
VOLUME /data
EXPOSE 3000
# the key comes from WAHA_API_KEY, the media link base from WAHA_BASE_URL, as in WAHA
ENTRYPOINT ["wago", "-addr", "0.0.0.0:3000", "-home", "/data"]
HEALTHCHECK --interval=15s --timeout=3s CMD wget -qO- http://127.0.0.1:3000/health >/dev/null || exit 1
