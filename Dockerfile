# syntax=docker/dockerfile:1
FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/forge ./cmd/forge

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 forge \
    && adduser -D -H -u 10001 -G forge forge
COPY --from=build /out/forge /usr/local/bin/forge
USER 10001:10001
ENV FORGE_ADDR=0.0.0.0:8080
EXPOSE 8080 9091
ENTRYPOINT ["/usr/local/bin/forge"]
CMD ["-mode", "api"]
