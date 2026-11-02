# Deployment strategy

Tick = done. Each line is one step, in order.

## Code (done)
- [x] Secrets: one JSON, local file in dev, Google Secret Manager in prod (`secret_sample.json` lists the names)
- [x] Database moved from SQLite to Postgres (sqlc, local Docker Postgres via `make docker_db_up`)
- [x] Audio goes to an object store (local disk in dev, Cloudflare R2 in prod), timestamp-named, presigned links
- [x] Telegram adapter uses a webhook (no long polling), checks secret header, dedupes updates in the database
- [x] Idle manager: window after a blog 6 min, first answer 10 min, each later answer keeps 5 min from now, cap 90 min
- [x] Production logging: JSON on stdout for Cloud Logging, `log.level` switch
- [x] Lean image (static binary + static ffmpeg on distroless, about 65 MB compressed)
- [x] API Gateway spec exposing only `/telegram` and `/keepalive`, both behind an API key
- [x] Pulumi program in `deploy/` (GCP project, Cloud Run, Gateway, Secret Manager, Artifact Registry, R2) with mock tests

## Neon database (done)
- [x] You signed in to Neon (`neon login`)
- [x] Created Neon project `sevi` (aws-us-east-1) with role `sevi` and database `sevi`; credentials saved to `deploy/neon.json` (git-ignored)
- [x] Applied the 7 migrations with `sevi migrate` (also runs as the Pulumi `db-migrate` step)

## Before the first deploy (you)
- [ ] Install the Pulumi CLI
- [ ] `gcloud auth login` and `gcloud auth application-default login`
- [ ] Get the Cloudflare account ID and create an API token (R2 edit, API tokens edit)
- [ ] Find the GCP billing account ID (`gcloud billing accounts list`) and pick a new project ID
- [ ] Create `config.prod.json` (dev false, listen 0.0.0.0:8080, telegram enabled, your user id)
- [ ] `pulumi login --local` and `pulumi stack init prod` in `deploy/`
- [ ] Set the Pulumi config values and secrets (project, billing, Cloudflare, OpenRouter key, bot token, database URL)

## Deploy
- [ ] Close heavy apps (the Go build of the Pulumi program needs about 4 GB)
- [ ] `make infra_preview` and read the plan
- [ ] `make infra_up` (project, APIs, registry, secret, image, Cloud Run, gateway, R2, webhook)
- [ ] Fix any first-run errors (quota project, Cloudflare permission names, gateway service account) and re-run

## Check it works
- [ ] Send the bot a blog link and get the MP3 back
- [ ] Ask a follow-up by voice and by text
- [ ] Call the gateway with no key, then the Cloud Run URL directly: both must be refused and start no instance
- [ ] Confirm Cloud Run scales to zero after the idle window and cold-starts cleanly
- [ ] Open Logs Explorer and find a request by its `ref`

## By hand, not in Pulumi
- [ ] GCP Billing budget alerts at $1 and $5 (email only; the kill switch function was dropped)
- [ ] Credit limit on the OpenRouter key

## Afterwards
- [ ] Update `readme.md`, `design/implementation.md` and `design/deploy.md` (still say Neon created by Pulumi and no object store)
- [ ] Add store, API-audio and adapter flow tests; run `govulncheck`
