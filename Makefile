BINARY_NAME=proc-compose
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: build install clean test

build:
	go build $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/proc-compose

install: build
	cp bin/$(BINARY_NAME) ~/.local/bin/$(BINARY_NAME)

clean:
	rm -rf bin/

test:
	go test ./... -race -count=1
