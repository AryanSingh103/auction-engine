# Developer entry points. `make help` lists targets.
#
# Config lives in .env (git-ignored; copy from .env.example). Only `run`
# reads it, by sourcing it in the shell. It is deliberately NOT loaded with
# make's `include`: make would expand `$` and treat `#` as a comment inside
# values, and exported makefile variables would leak into every recipe.

# Integration tests use testcontainers, which needs the Docker API socket.
# With colima (or any non-default docker context) it is not at
# /var/run/docker.sock on the host, so take it from the active context.
# Ryuk, testcontainers' cleanup container, mounts the socket from inside the
# Docker VM, where it IS at /var/run/docker.sock. `?=` keeps any value
# already set in the environment (CI sets neither and uses the defaults).
export DOCKER_HOST ?= $(shell docker context inspect -f '{{.Endpoints.docker.Host}}' 2>/dev/null)
export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE ?= /var/run/docker.sock

.DEFAULT_GOAL := help

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-12s %s\n", $$1, $$2}'

.PHONY: run
run: ## Run the API on the host with config from .env
	@test -f .env || { echo ".env not found: cp .env.example .env"; exit 1; }
	set -a && . ./.env && set +a && go run ./cmd/api

.PHONY: migrate
migrate: ## Apply pending migrations to the database in .env (host)
	@test -f .env || { echo ".env not found: cp .env.example .env"; exit 1; }
	set -a && . ./.env && set +a && go run ./cmd/migrate up

.PHONY: migrate-status
migrate-status: ## Show applied and pending migrations (host)
	@test -f .env || { echo ".env not found: cp .env.example .env"; exit 1; }
	set -a && . ./.env && set +a && go run ./cmd/migrate status

.PHONY: build
build: ## Build the api, migrate and loadgen binaries into bin/
	go build -o bin/ ./cmd/...

.PHONY: test
test: ## Run all tests
	go test ./...

.PHONY: test-race
test-race: ## Run all tests with the race detector, uncached
	go test -race -count=1 ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt-check
fmt-check: ## Fail if any file is not gofmt-formatted
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "not gofmt-formatted:"; echo "$$out"; exit 1; fi

.PHONY: lint
lint: ## Run golangci-lint (config in .golangci.yml)
	golangci-lint run ./...

.PHONY: up
up: ## Build and start the full stack; waits for postgres healthy, api running
	docker compose up -d --build --wait

.PHONY: down
down: ## Stop the stack (keeps the database volume)
	docker compose down

.PHONY: logs
logs: ## Follow logs from all services
	docker compose logs -f

.PHONY: psql
psql: ## Open psql inside the postgres container
	docker compose exec postgres sh -c 'psql -U "$$POSTGRES_USER" -d "$$POSTGRES_DB"'

.PHONY: nuke
nuke: ## Stop the stack AND delete the database volume (destroys all local data)
	docker compose down -v

.PHONY: bench-smoke
bench-smoke: ## 10s load run (20 bidders, one auction) inside compose; fails if its checks fail
	RATE_LIMIT_BIDS_PER_SECOND=1000000 RATE_LIMIT_BID_BURST=1000000 BID_MAX_IN_FLIGHT=1000000 docker compose up -d --wait --build api
	docker compose --profile bench run --rm --build loadgen --workers=20 --warmup=2s --duration=10s --out=- > /dev/null
	docker compose up -d --wait api

.PHONY: test-page
test-page: ## Check the live page's client contract (needs Node)
	node internal/httpapi/web/contract_test.js
