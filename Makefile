COMPOSE = docker compose -f localdb/docker-compose.yml

.PHONY: gateway_spec gcp_log_retention build_linux docker_build docker_db_up docker_db_down docker_db_logs docker_db_psql docker_db_reset

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

# Keep Cloud Logging's default bucket for 7 days (shorter than the 30-day default, so never billed).
# Usage: make gcp_log_retention PROJECT=my-project
gcp_log_retention:
	gcloud logging buckets update _Default --location=global --retention-days=7 --project=$(PROJECT)

# Render the API Gateway spec with the real Cloud Run URL (no trailing slash).
# Usage: make gateway_spec CLOUD_RUN_URL=https://sevi-xxxx-uc.a.run.app
gateway_spec:
	@test -n "$(CLOUD_RUN_URL)" || (echo "set CLOUD_RUN_URL=https://..." && exit 1)
	@mkdir -p bin
	sed 's|CLOUD_RUN_URL|$(CLOUD_RUN_URL)|g' deploy/openapi.yaml > bin/openapi.rendered.yaml
	@echo wrote bin/openapi.rendered.yaml
