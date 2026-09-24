# Developer entry points. `make help` lists targets.
#
# .env (git-ignored; copy from .env.example) is loaded into make variables and
# exported to every recipe, so `make run` sees the same configuration docker
# compose does. The leading dash means a missing .env is not an error for
# targets that do not need it (test, lint, ...); the app itself fails fast on
# missing config.
-include .env
export

.DEFAULT_GOAL := help

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-12s %s\n", $$1, $$2}'

.PHONY: run
run: ## Run the API on the host with config from .env
	go run ./cmd/api

.PHONY: build
build: ## Build the API binary to bin/api
	go build -o bin/api ./cmd/api

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
up: ## Build and start the full stack in docker compose, wait until healthy
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
