BINARY      := kai-mcp-server
PKG         := github.com/jaiakash/k8s-ai-agent
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
IMAGE       ?= ghcr.io/jaiakash/kai-mcp-server
PLATFORMS   ?= linux/amd64,linux/arm64

.DEFAULT_GOAL := check

.PHONY: build
build: ## Build the server into ./bin
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

.PHONY: install
install: ## Install the server into $GOBIN
	go install -ldflags "$(LDFLAGS)" ./cmd/$(BINARY)

.PHONY: test
test: ## Run all tests with the race detector
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests and open a coverage report
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

.PHONY: lint
lint: ## Check formatting and run go vet
	@unformatted=$$(gofmt -l . 2>/dev/null); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod and fail if it changed
	go mod tidy
	@git diff --exit-code go.mod go.sum

.PHONY: check
check: lint test ## Everything CI runs

.PHONY: run
run: build ## Run on stdio against the current kubeconfig (read-only)
	./bin/$(BINARY)

.PHONY: run-http
run-http: build ## Run the Streamable HTTP transport on :8080
	./bin/$(BINARY) --transport http --addr :8080

.PHONY: docker
docker: ## Build the container image for the host platform
	docker build -t $(IMAGE):$(VERSION) --build-arg VERSION=$(VERSION) .

.PHONY: docker-push
docker-push: ## Build and push a multi-arch image
	docker buildx build --platform $(PLATFORMS) \
		-t $(IMAGE):$(VERSION) --build-arg VERSION=$(VERSION) --push .

.PHONY: helm-lint
helm-lint: ## Lint and render the chart
	helm lint deploy/helm/kai
	helm template kai deploy/helm/kai >/dev/null

.PHONY: clean
clean: ## Remove build output
	rm -rf bin coverage.out

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
