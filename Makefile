SHELL := /bin/bash
.DEFAULT_GOAL := help

MODULE  := url_shortener
IMAGE   ?= url-shortener
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(MODULE)/internal/buildinfo.Version=$(VERSION)

# Exports every variable in .env into the recipe's shell, the same way the
# container receives them, so the binaries themselves only ever read the
# process environment.
WITH_ENV := set -a && source .env && set +a &&

.PHONY: help
help: ## List available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: generate
generate: ## Regenerate the ent client from ent/schema
	go generate ./ent

.PHONY: build
build: ## Build all binaries into ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

.PHONY: run
run: ## Run the API locally with .env loaded
	$(WITH_ENV) go run ./cmd/api

.PHONY: migrate
migrate: ## Apply the ent schema to the database in .env
	$(WITH_ENV) go run ./cmd/migrate

.PHONY: test
test: ## Run all tests with the race detector
	go test -race -count=1 ./...

.PHONY: lint
lint: ## Check formatting and run go vet
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "files above need gofmt"; exit 1; }
	go vet ./...

.PHONY: tidy
tidy: ## Sync go.mod and go.sum with the imports
	go mod tidy

.PHONY: docker-build
docker-build: ## Build the container image for the local platform
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

# k6 runs in Docker, so nothing needs installing. It reaches the API on the
# host through host.docker.internal. Pass k6 settings through K6_ENV, e.g.
#   make loadtest SCRIPT=redirect K6_ENV="-e RATE=500 -e DURATION=30s"
SCRIPT ?= redirect
K6_ENV ?=

.PHONY: loadtest
loadtest: ## Run a k6 load test from ./loadtest (SCRIPT=redirect|shorten)
	$(WITH_ENV) docker run --rm -i \
		--add-host=host.docker.internal:host-gateway \
		-v "$(CURDIR)/loadtest:/scripts:ro" \
		-e BASE_URL=http://host.docker.internal:8080 \
		-e ADMIN_TOKEN="$$ADMIN_TOKEN" \
		grafana/k6 run $(K6_ENV) /scripts/$(SCRIPT).js

.PHONY: loadtest-clean
loadtest-clean: ## Delete every link created by the load tests
	$(WITH_ENV) docker run --rm postgres:17-alpine psql "$$DATABASE_URL" \
		-c "DELETE FROM links WHERE original_url LIKE 'https://example.com/loadtest/%'"

.PHONY: up
up: ## Migrate and run the API in Docker via compose
	docker compose up --build
