.PHONY: help build test test-int lint fmt migrate-up run-api run-worker dev-up dev-down

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build:      ## Build all binaries
	go build ./...

test:       ## Unit tests (no external dependencies, must stay fast)
	go test ./internal/... -race -count=1

test-int:   ## Integration tests (spins real Postgres via testcontainers)
	go test ./test/... -race -count=1 -tags=integration

lint:       ## Lint, including the architecture import rules
	golangci-lint run ./...

fmt:
	gofmt -w . && go mod tidy

migrate-up: ## Apply migrations
	go run ./cmd/migrate up

run-api:
	go run ./cmd/api

run-worker:
	go run ./cmd/worker

dev-up:     ## Start Postgres + Redis for local development
	docker compose -f deploy/docker-compose.yml up -d

dev-down:
	docker compose -f deploy/docker-compose.yml down -v
