VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint docker
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/repohub ./cmd/repohub
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/rh ./cmd/rh

test:
	go test -race -count=1 ./...

lint:
	test -z "$$(gofmt -l .)"
	go vet ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t repohub:$(VERSION) .
