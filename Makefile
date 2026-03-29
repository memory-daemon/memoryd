.PHONY: build tray app run clean release validate corpus-eval

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION) -s -w

build:
	go build -ldflags "$(LDFLAGS)" -o bin/memoryd ./cmd/memoryd

tray:
	go build -o bin/memoryd-tray ./cmd/memoryd-tray

app: build tray
	./scripts/build-app.sh

run: build
	./bin/memoryd start

clean:
	rm -rf bin/ dist/

validate:
	go run ./cmd/validate

corpus-eval:
	go run ./cmd/corpus-eval

release: clean
	mkdir -p dist
	# macOS ARM64
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/darwin-arm64/memoryd ./cmd/memoryd
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/darwin-arm64/memoryd-tray ./cmd/memoryd-tray
	./scripts/build-release-app.sh dist/darwin-arm64
	tar -czf dist/memoryd_$(VERSION)_darwin_arm64.tar.gz -C dist/darwin-arm64 memoryd Memoryd.app
	# macOS AMD64 (CLI only — tray requires native CGo)
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/darwin-amd64/memoryd ./cmd/memoryd
	tar -czf dist/memoryd_$(VERSION)_darwin_amd64.tar.gz -C dist/darwin-amd64 memoryd
	# Linux AMD64
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/linux-amd64/memoryd ./cmd/memoryd
	tar -czf dist/memoryd_$(VERSION)_linux_amd64.tar.gz -C dist/linux-amd64 memoryd
	# Linux ARM64
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/linux-arm64/memoryd ./cmd/memoryd
	tar -czf dist/memoryd_$(VERSION)_linux_arm64.tar.gz -C dist/linux-arm64 memoryd
	@echo "Release archives in dist/"
	@ls -lh dist/*.tar.gz
