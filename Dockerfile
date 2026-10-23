FROM golang:1.24-alpine AS builder
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" -o /fleet-manager .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates git \
 && adduser -D -u 1000 fleet \
 && mkdir /data && chown fleet /data
COPY --from=builder /fleet-manager /usr/local/bin/fleet-manager
USER 1000:1000
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://127.0.0.1:8080/healthz || wget -qO- --no-check-certificate https://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/fleet-manager"]
