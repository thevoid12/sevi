COMPOSE = docker compose -f localdb/docker-compose.yml

.PHONY: build_linux docker_build docker_db_up docker_db_down docker_db_logs docker_db_psql docker_db_reset

# Start local Postgres and wait until it is healthy.
docker_db_up:
	$(COMPOSE) up -d --wait

# Stop it; data is kept in the volume.
docker_db_down:
	$(COMPOSE) down

docker_db_logs:
	$(COMPOSE) logs -f db

docker_db_psql:
	$(COMPOSE) exec db psql -U sevi -d sevi

# Stop it AND delete all local data (migrations rerun on next start).
docker_db_reset:
	$(COMPOSE) down -v
	$(COMPOSE) up -d --wait

# Static linux/amd64 binary (no CGO) for the production image.
build_linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/sevi ./cmd/sevi

# Build the binary, then the production image.
docker_build: build_linux
	docker build -t sevi:local .
