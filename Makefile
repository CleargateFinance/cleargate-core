.PHONY: help check build image test test-int test-demo lint fmt migrate-up migrate-down migrate-version run-api run-worker dev-up dev-down

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# Docker must be running: test-int starts real Postgres and Redis.
check:          ## Everything a change must pass: gofmt, vet, lint, unit and integration tests
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed on:"; gofmt -l .; exit 1; }
	go vet -tags=integration ./...
	$(MAKE) lint test test-int test-demo

build:          ## Build all binaries
	go build ./...

# linux/arm64 is the Fargate target (Graviton). The build cross-compiles, so it
# needs no emulator, even on an x86 machine.
IMAGE   ?= cleargate-core
GIT_SHA ?= $(shell git rev-parse HEAD 2>/dev/null || echo dev)

image:          ## Build the container image (api, worker, migrate) for linux/arm64
	docker buildx build --platform linux/arm64 --build-arg GIT_SHA=$(GIT_SHA) -t $(IMAGE):$(GIT_SHA) --load .

test:           ## Unit tests (no external dependencies, must stay fast)
	go test ./internal/... -race -count=1

# Integration tests live in two places: test/ holds end-to-end tests, and
# internal/ holds package-level tests that need a real database. Both carry the
# integration build tag, so ./... covers them and skips everything else.
test-int:       ## Integration tests (spins real Postgres + Redis via testcontainers)
	go test ./... -race -count=1 -tags=integration

# demo/ is its own Go module, so ./... from the root never reaches it.
test-demo:      ## Vet and unit-test the demo in demo/ (no external dependencies)
	cd demo && go vet ./... && go test ./... -race -count=1

lint:           ## Lint, including the architecture import rules, and the demo
	golangci-lint run ./...
	cd demo && golangci-lint run ./...

fmt:
	gofmt -w . && go mod tidy && cd demo && go mod tidy

migrate-up:     ## Apply every pending migration
	go run ./cmd/migrate up

migrate-down:   ## Roll back exactly one migration
	go run ./cmd/migrate down

migrate-version: ## Print the current schema version
	go run ./cmd/migrate version

run-api:
	go run ./cmd/api

run-worker:
	go run ./cmd/worker

dev-up:         ## Start Postgres + Redis for local development
	docker compose -f deploy/docker-compose.yml up -d

dev-down:
	docker compose -f deploy/docker-compose.yml down -v
