VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build install test clean vz agent menu uffd all npm-sync

build:
	go build -ldflags "$(LDFLAGS)" -o bin/mvm ./cmd/mvm

agent:
	cd agent && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "-s -w" -o ../bin/mvm-agent .

vz:
	cd vz && swift build -c release
	cp vz/.build/release/mvm-vz bin/mvm-vz

menu:
	CGO_ENABLED=1 go build -ldflags "$(LDFLAGS)" -o bin/mvm-menu ./cmd/mvm-menu

uffd:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/mvm-uffd-amd64 ./cmd/mvm-uffd
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/mvm-uffd-arm64 ./cmd/mvm-uffd

all: build agent vz menu uffd

install: build vz
	go install -ldflags "$(LDFLAGS)" ./cmd/mvm
	cp bin/mvm-vz $(shell go env GOPATH)/bin/mvm-vz

test:
	go test ./internal/... -v -race

npm-sync:
	@TAG=$$(git describe --tags --abbrev=0 2>/dev/null) && \
	VER=$${TAG#v} && \
	sed -i '' "s/\"version\": \".*\"/\"version\": \"$$VER\"/" npm/package.json && \
	echo "npm/package.json → $$VER"

clean:
	rm -rf bin/
	rm -rf vz/.build/
