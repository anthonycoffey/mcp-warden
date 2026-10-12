VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/anthonycoffey/mcp-warden/internal/cli.Version=$(VERSION)
BIN     := bin/mcp-warden$(if $(filter Windows_NT,$(OS)),.exe,)

.PHONY: build test race cover bench lint vet cross clean

build: ## Build the binary into ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/mcp-warden

test: ## Unit + end-to-end tests
	go test ./...

race: ## Tests with the race detector (needs cgo / a C compiler)
	CGO_ENABLED=1 go test -race ./...

cover: ## Coverage report
	go test -coverprofile=coverage.out ./internal/...
	go tool cover -func=coverage.out | tail -1

bench: ## Proxy per-message overhead
	go test -run='^$$' -bench=. -benchmem ./internal/proxy/

vet:
	go vet ./...
	GOOS=linux go vet ./...
	GOOS=darwin go vet ./...

lint: ## golangci-lint (https://golangci-lint.run)
	golangci-lint run

cross: ## Build every release target
	@for t in windows/amd64 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		GOOS=$${t%/*} GOARCH=$${t#*/} CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/mcp-warden-$${t%/*}-$${t#*/}$$( [ $${t%/*} = windows ] && echo .exe ) ./cmd/mcp-warden && echo "built $$t"; \
	done

clean:
	rm -rf bin dist coverage.out
