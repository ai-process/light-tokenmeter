FROM golang:1.27.3-alpine3.23 AS builder
RUN apk add --no-cache 'ca-certificates>=20261020-r0'
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/light-tokenmeter ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/light-tokenmeter /light-tokenmeter
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532
EXPOSE 8080 9090
ENTRYPOINT ["/light-tokenmeter"]
