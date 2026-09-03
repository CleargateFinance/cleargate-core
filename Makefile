.PHONY: help build test test-int lint fmt migrate-up migrate-down migrate-version run-api run-worker dev-up dev-down

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

build:          ## Build all binaries
	go build ./...

test:           ## Unit tests (no external dependencies, must stay fast)
	go test ./internal/... -race -count=1

# Integration tests live in two places: test/ holds end-to-end tests, and
# internal/ holds package-level tests that need a real database. Both carry the
# integration build tag, so ./... covers them and skips everything else.
test-int:       ## Integration tests (spins real Postgres + Redis via testcontainers)
	go test ./... -race -count=1 -tags=integration

lint:           ## Lint, including the architecture import rules
	golangci-lint run ./...

fmt:
	gofmt -w . && go mod tidy

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
