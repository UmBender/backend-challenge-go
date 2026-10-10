COMPOSE ?= docker compose
MIGRATE  = $(COMPOSE) run --rm migrate -path=/migrations \
	-database=postgres://wallet_owner:$${WALLET_OWNER_PASSWORD:-wallet_owner}@postgres:5432/wallet?sslmode=disable

.PHONY: help up down down-volumes ps logs migrate-up migrate-down migrate-version \
	fmt fmt-check vet test test-race test-integration check

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

up: ## Build and start everything in the background
	$(COMPOSE) up --build -d

down: ## Stop containers, keep the database volume
	$(COMPOSE) down

down-volumes: ## Stop containers and delete the database volume (re-runs initdb)
	$(COMPOSE) down -v

ps: ## Show container and health status
	$(COMPOSE) ps

logs: ## Follow logs of the three wallet replicas
	$(COMPOSE) logs -f wallet-1 wallet-2 wallet-3

migrate-up: ## Apply all pending migrations
	$(MIGRATE) up

migrate-down: ## Revert the most recent migration
	$(MIGRATE) down 1

migrate-version: ## Print the current migration version
	$(MIGRATE) version

fmt: ## Format all Go code
	gofmt -w .

fmt-check: ## Fail if any Go file is not gofmt-formatted
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run: make fmt" && exit 1)

vet: ## go vet
	go vet ./...
	go vet -tags=integration ./...

test: ## Unit tests (no Docker needed)
	go test ./...

test-race: ## Unit tests with the race detector
	go test -race ./...

test-integration: ## Integration tests; needs `make up` (or the infra services) running
	go test -tags=integration -race -count=1 ./...

check: fmt-check vet test-race ## Everything that runs without Docker
