.PHONY: build run test test-coverage proto proto-lint fmt clean

BINARY_NAME=light-tokenmeter
GO=go
GOFLAGS=-trimpath -ldflags="-s -w"

build:
	$(GO) build $(GOFLAGS) -o bin/$(BINARY_NAME) ./cmd/server

run: build
	./bin/$(BINARY_NAME)

test:
	$(GO) test -race ./...

test-coverage:
	$(GO) test -race -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

# Regenerate gen/ after editing proto/ (needs buf).
proto:
	buf generate

proto-lint:
	buf lint

fmt:
	$(GO) fmt ./...

clean:
	rm -rf bin/ coverage.out coverage.html
