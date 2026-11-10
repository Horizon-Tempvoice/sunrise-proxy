VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null)
BUILDINFO := github.com/Horizon-Tempvoice/sunrise-proxy/internal/buildinfo
LDFLAGS   := -s -w -X $(BUILDINFO).Version=$(VERSION) -X $(BUILDINFO).Commit=$(COMMIT)

IMAGE ?= ghcr.io/horizon-tempvoice/sunrise-proxy

# renovate: datasource=github-releases depName=golangci/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.15.0

COMPOSE := VERSION=$(VERSION) COMMIT=$(COMMIT) docker compose
CLUSTER := $(COMPOSE) -f docker-compose.clustered-example.yaml

.DEFAULT_GOAL := help
.PHONY: help build run test vet lint fmt check docker up down logs cluster-up cluster-down cluster-logs clean

help: ## Show the available targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "} {printf "  %-13s %s\n", $$1, $$2}'

build: ## Build bin/sunrise
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/sunrise ./cmd/sunrise

run: build ## Build and run Sunrise locally (reads .env)
	./bin/sunrise

test: ## Run the tests with the race detector
	go test -race -count=1 ./...

vet: ## Run go vet
	go vet ./...

lint: ## Run golangci-lint in the version CI uses
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

fmt: ## Format the Go sources
	gofmt -w cmd internal

check: vet test lint ## Run what CI runs

docker: ## Build the Docker image locally
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE):latest .

up: ## Start one node with Docker Compose
	$(COMPOSE) up -d --build

down: ## Stop the single node
	$(COMPOSE) down

logs: ## Follow the single node's logs
	$(COMPOSE) logs -f

cluster-up: ## Start the three node example cluster
	$(CLUSTER) up -d --build

cluster-down: ## Stop the example cluster
	$(CLUSTER) down

cluster-logs: ## Follow the example cluster's logs
	$(CLUSTER) logs -f

clean: ## Remove build output
	rm -rf bin
