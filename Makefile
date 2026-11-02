COMPOSE = docker compose -f localdb/docker-compose.yml
# deploy/setup.sh stores the Pulumi passphrase here, so the targets below never prompt for it.
PULUMI_ENV = PATH="$$HOME/.pulumi/bin:$$PATH" PULUMI_CONFIG_PASSPHRASE_FILE="$$PWD/.passphrase"

.PHONY: infra_check infra_preview infra_up infra_destroy build_linux docker_build docker_db_up docker_db_down docker_db_logs docker_db_psql docker_db_reset

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

# Infrastructure (Pulumi, see deploy/README.md). infra_up builds the binary first
# because the image copies bin/sevi.
# Stop early, with a clear message, if deploy/setup.sh has not been run.
infra_check:
	@test -s deploy/.passphrase || (echo "Run deploy/setup.sh first (it logs in to Pulumi and sets the config)." && exit 1)

infra_preview: infra_check build_linux
	cd deploy && $(PULUMI_ENV) pulumi preview

infra_up: infra_check build_linux
	cd deploy && $(PULUMI_ENV) pulumi up

infra_destroy: infra_check
	cd deploy && $(PULUMI_ENV) pulumi destroy
