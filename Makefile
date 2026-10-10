COMPOSE ?= docker compose
MIGRATE  = $(COMPOSE) run --rm migrate -path=/migrations \
	-database=postgres://wallet_owner:$${WALLET_OWNER_PASSWORD:-wallet_owner}@postgres:5432/wallet?sslmode=disable

.PHONY: help up down down-volumes ps migrate-up migrate-down migrate-version \
	fmt fmt-check vet test test-race test-integration check

help:
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

up:
	$(COMPOSE) up --build -d

down:
	$(COMPOSE) down

down-volumes:
	$(COMPOSE) down -v

ps:
	$(COMPOSE) ps

migrate-up:
	$(MIGRATE) up

migrate-down:
	$(MIGRATE) down 1

migrate-version:
	$(MIGRATE) version

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run: make fmt" && exit 1)

vet:
	go vet -tags=integration ./...

test:
	go test ./...

test-race:
	go test -race ./...

test-integration:
	go test -tags=integration -race -count=1 ./...

check: fmt-check vet test-race
