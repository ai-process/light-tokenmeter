FROM golang:1.27-alpine AS builder
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
USER 65532:65532
EXPOSE 8080 9090
ENTRYPOINT ["/light-tokenmeter"]
