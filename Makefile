.PHONY: build test lint

build:
	go build ./...

test:
	go test ./...

lint:
	@if which golangci-lint > /dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed; skipping lint"; \
	fi
