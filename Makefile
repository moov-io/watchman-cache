.PHONY: help up down teardown restart logs test test-short clean pull

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-15s\033[0m %s\n", $$1, $$2}'

pull: ## Pull the pinned watchman image
	docker compose pull watchman

up: ## Build (if needed) and start cache + watchman (waits only for cache; watchman data load takes 30-120s)
	docker compose up -d --build --wait --wait-timeout 60 cache
	# --no-deps keeps Compose from recreating the cache container, which races its healthcheck.
	docker compose up -d --no-deps watchman
	@echo "Cache is ready. Watchman is starting (use 'make ping' or 'docker compose logs -f watchman' to observe data load via cache)"

down: ## Stop containers and keep the cache volume
	docker compose stop

teardown: ## Remove containers, the cache volume, and the local image
	docker compose down -v --remove-orphans
	docker rmi watchman-cache:local 2>/dev/null || true

restart: ## Restart the stack
	docker compose restart

logs: ## Tail logs from both services
	docker compose logs -f --tail=100

logs-cache: ## Tail only nginx cache logs (shows cache hits/misses)
	docker compose logs -f cache

logs-watchman: ## Tail only watchman logs (shows download progress)
	docker compose logs -f watchman

test: ## Run the Go integration test (brings stack up, verifies watchman starts via cache)
	# 15m covers a cold watchman image pull plus one restart if consolidated.csv
	# is truncated on the first fetch.
	go test -v -run TestWatchmanStartsThroughCache -count=1 -timeout 15m .

test-short: ## Run unit tests only (skips integration)
	go test -short -v ./...

clean: teardown ## Remove generated files and docker resources
	rm -rf /tmp/watchman-* 2>/dev/null || true

# Quick manual verification targets
ping: ## Hit watchman /ping (stack must be up)
	curl -sS http://localhost:8084/ping || echo "watchman not responding on :8084"

cache-health: ## Hit nginx cache /health
	curl -sS http://localhost:3000/health || echo "cache not responding on :3000"

cache-status: ## Show recent cache activity (hits/misses)
	docker compose exec cache sh -c 'tail -n 30 /var/log/nginx/access.log 2>/dev/null || echo "no logs yet"'
