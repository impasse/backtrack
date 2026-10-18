FROM node:24-alpine AS web-build
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run check && npm run build

FROM golang:1.27-alpine AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/dvr-server ./cmd/dvr-server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates ffmpeg tzdata
WORKDIR /app
COPY --from=go-build /out/dvr-server /usr/local/bin/dvr-server
COPY --from=web-build /app/web/build /app/web/build
COPY configs/dvr.example.yaml /app/configs/dvr.example.yaml
EXPOSE 8080 1935 50000-50100/udp
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/dvr-server"]
CMD ["-config", "/app/configs/dvr.yaml"]
