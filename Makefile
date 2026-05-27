.PHONY: help up down restart logs test test-short clean pull

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-15s\033[0m %s\n", $$1, $$2}'

pull: ## Pull the pinned watchman image
	docker compose pull watchman

up: ## Build (if needed) and start cache + watchman (waits only for cache; watchman data load takes 30-120s)
	docker compose up -d --build --wait --wait-timeout 60 cache
	docker compose up -d watchman
	@echo "Cache is ready. Watchman is starting (use 'make ping' or 'docker compose logs -f watchman' to observe data load via cache)"

down: ## Stop and remove containers + volumes
	docker compose down -v

restart: ## Restart the stack
	docker compose restart

logs: ## Tail logs from both services
	docker compose logs -f --tail=100

logs-cache: ## Tail only nginx cache logs (shows cache hits/misses)
	docker compose logs -f cache

logs-watchman: ## Tail only watchman logs (shows download progress)
	docker compose logs -f watchman

test: ## Run the Go integration test (brings stack up, verifies watchman starts via cache)
	# 8m total allows for occasional cold-start truncation on consolidated.csv
	# (Azure origin flakes) + one watchman fatal/restart + successful second load.
	go test -v -run TestWatchmanStartsThroughCache -count=1 -timeout 8m .

test-short: ## Run unit tests only (skips integration)
	go test -short -v ./...

clean: down ## Remove generated files and docker resources
	rm -rf /tmp/watchman-v0.62.0 2>/dev/null || true
	docker compose down -v --remove-orphans 2>/dev/null || true
	docker rmi watchman-cache:local 2>/dev/null || true

# Quick manual verification targets
ping: ## Hit watchman /ping (stack must be up)
	curl -sS http://localhost:8084/ping || echo "watchman not responding on :8084"

cache-health: ## Hit nginx cache /health
	curl -sS http://localhost:3000/health || echo "cache not responding on :3000"

cache-status: ## Show recent cache activity (hits/misses)
	docker compose exec cache sh -c 'tail -n 30 /var/log/nginx/access.log 2>/dev/null || echo "no logs yet"'
