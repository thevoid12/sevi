# Deploying sevi

`make infra_up` builds `bin/sevi` and runs this Pulumi program, which creates the
GCP project (Cloud Run, API Gateway + API key, Secret Manager, Artifact Registry,
7-day log retention, a billing budget alert), the Cloudflare R2 audio bucket (files expire after
`retention_days` from `config.prod.json`, and the app deletes database rows older than that too), applies the database schema to your Neon database, builds and
pushes the image, and points Telegram's webhook at the gateway.

## One-time setup

Install `pulumi`, `gcloud` and `docker`, then:

```sh
gcloud auth login
gcloud auth application-default login      # Pulumi and `sevi telegram set-webhook` use this
cd deploy
pulumi login --local                        # or `pulumi login` for Pulumi Cloud
pulumi stack init prod

pulumi config set sevi:projectId <new-project-id>
pulumi config set sevi:billingAccount <id>            # gcloud billing accounts list
pulumi config set sevi:cloudflareAccountId <id>
pulumi config set --secret sevi:openrouterApiKey <key>
pulumi config set --secret sevi:telegramBotToken <token>
pulumi config set --secret sevi:databaseUrl "$(python3 -c 'import json;print(json.load(open("neon.json"))["DATABASE_URL"])')"
pulumi config set --secret cloudflare:apiToken <cloudflare token>
pulumi config set gcp:userProjectOverride true      # see the first-run notes below
pulumi config set gcp:billingProject <new-project-id>
pulumi config set gcp:disableGlobalProjectWarning true
```

Also create `config.prod.json` in the repo root (copy `config.example.json`; set
`app.dev` false, `app.listen` `0.0.0.0:8080`, `api.allow_remote_listen` true,
`telegram.enabled` true and your `telegram.allowed_user_ids`). It is baked into the image.

Then `make infra_up` from the repo root.

## Not done by Pulumi (set by hand)

- A **credit limit** on the OpenRouter key.
- Spending caps: the budget in Pulumi only sends email (to the billing account's admins) from the first paid cent. It does not stop anything, and it covers the GCP project only, not R2, Neon or OpenRouter.
- `pulumi destroy` removes the Telegram webhook, but R2 buckets with objects may need emptying first.

## If the first run fails

- *"API ... requires a quota project"* or *"has not been used in project ..."*: your login's quota
  project is not the new project. The three `gcp:` settings above fix it; run `make infra_up` again.
- *"Artifact Registry API has not been used"* right after creating the project: Google needs a minute
  or two to propagate a newly enabled API. Wait and run `make infra_up` again.
- *"Please enable R2 through the Cloudflare Dashboard"*: open R2 in the Cloudflare dashboard and
  subscribe once (the free tier is 10 GB, and it costs nothing under the monthly limits).
- Building the Go program needs about 4 GB of memory (the GCP SDK is large).
- *"API ... is not enabled for the project"* (403 `SERVICE_NOT_ACTIVATED`) from the gateway: the API's managed
  service is enabled by Pulumi (`api-managed-service`); re-run `make infra_up` if it is missing.
- Pulumi continues where it stopped, so read the error, fix, re-run.
