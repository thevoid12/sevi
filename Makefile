COMPOSE = docker compose -f localdb/docker-compose.yml

.PHONY: docker_db_up docker_db_down docker_db_logs docker_db_psql docker_db_reset

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
