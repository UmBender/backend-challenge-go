COMPOSE ?= docker compose
.PHONY: help up down down-volumes ps fmt fmt-check vet test test-race check

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

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run: make fmt" && exit 1)

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

check: fmt-check vet test-race
