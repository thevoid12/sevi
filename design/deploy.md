# Plan: Deploy sevi at $0 (API Gateway + Cloud Run + Neon Postgres)

## Context
Usage is 3 runs a day, 2-3 days a week: about 25-40 sessions a month, roughly 2 hours of compute. Every free tier involved covers that many times over, so cost is not the hard constraint. The design optimises for three things instead: no bill is possible (kill switch everywhere), strangers can never start an instance, and as little new code as possible.

The Go binary (with ffmpeg) runs in one Cloud Run container. The database moves from SQLite to Neon Postgres, reached with the pgx driver. Telegram reaches the container through Google API Gateway, which checks an API key before anything starts. Cloudflare (D1, R2) and Turso are not used.

## Decisions (and what they replaced)
| Topic | Decision | Replaced |
|---|---|---|
| Database | Neon Postgres, free plan, no payment method on file. Go side: `pgx/v5` (`pgxpool`) and sqlc's `postgresql` engine. Real transactions, pure Go, no CGO. | D1 over REST, then Turso (its Go client's upkeep is doubtful) |
| Dev and tests | Postgres everywhere: local Postgres container for dev and `TestLinksEndToEnd`, optionally a free Neon branch. SQLite and `mattn/go-sqlite3` are removed. | SQLite file in dev, remote DB in prod (two dialects hide bugs) |
| Audio storage | No blob store in production. `audio_cache` stores Telegram's `file_id`; re-sending a blog is a free `sendAudio(file_id)`. The blob interface stays (local disk for dev); object storage comes with the web adapter. | R2 |
| Front door | API Gateway checks `?key=K`; Cloud Run is IAM-only and only the gateway's service account may call it. | (kept) |
| Second lock | Go checks Telegram's `X-Telegram-Bot-Api-Secret-Token` in constant time. | (kept) |
| Background work | Reply 200 to Telegram in milliseconds, run the job in a goroutine. Needs instance-based billing (`--no-cpu-throttling`). | Cloud Tasks considered and dropped as not worth it at this usage |
| Instance lifetime | Idle manager keeps the instance (and Neon) warm through the listening and follow-up window with self-pings, then exits. | Idle manager that only shortened lifetime |
| State | Everything that must survive a restart is in Postgres: `pending` Yes/No prompts and seen `update_id`s. Running jobs stay in memory (lost on a kill; resend the link, caches make it cheap). | `pending` and dedupe in memory |
| Secrets | One JSON secret in Secret Manager. | Six separate secrets |
| Money | Hard kill switches: GCP budget disables billing, Neon free plan has no card to bill, OpenRouter key has a credit limit. | $1 budget alert (email only) |

## Architecture
See `design/deploy-architecture.svg`. In text:

```
You (Telegram app)
   │
Telegram servers ──POST /telegram?key=K + secret header──► API Gateway ──(SA token)──► Cloud Run "sevi"
                                                           │ bad/missing key:            (IAM-only, min 0, max 1,
Stranger ──────────────────────────────────────────────────┘ rejected, nothing starts     CPU always on, 1 vCPU / 1 GiB)
Stranger ──direct run.app URL──► 403 from Google IAM, nothing starts                      │
                                                                                           ├─ webhook handler → 200 in ms
                                                 ┌── self-ping /keepalive?key=K ◄──────────┤─ idle manager (+ SELECT 1)
                                                 ▼                                         ├─ Core: jobs as goroutines, ffmpeg in /tmp (RAM)
                                            API Gateway                                    │
                                                                                           ├──► api.telegram.org  (sendMessage, editMessageText,
                                                                                           │                       sendAudio / sendVoice, getFile)
                                                                                           ├──► OpenRouter        (chat, TTS, price list)
                                                                                           ├──► Jina Reader       (page → text)
                                                                                           └──► Neon Postgres     (all tables, pgx over TLS)
Secret Manager (one JSON) · Artifact Registry (image) · Budget → Pub/Sub → billing kill switch
```

## Request flow
1. You send a link or a voice note. Telegram POSTs the update to `https://<gateway>/telegram?key=K` with the `secret_token` header.
2. The gateway checks the key. A missing or wrong key is rejected there and Cloud Run is never contacted. A valid one is forwarded with the gateway service account's token. A request straight to the `run.app` URL gets 403 from Google IAM and starts nothing.
3. If no instance is running, Cloud Run cold-starts one (time to be measured in the spike). Nothing is restored at boot; the database is remote.
4. The webhook handler checks the secret header, caps the body at 64 KB, checks the `from.id` allowlist (200 on miss so Telegram does not retry), records `update_id` in Postgres with `INSERT ... ON CONFLICT DO NOTHING` (zero rows affected means a duplicate: 200, nothing else), then replies 200.
5. A goroutine runs the job. Status goes to Telegram as separate outgoing calls: `sendMessage` once, then `editMessageText` as each step finishes, then `sendAudio` (blog) or `sendVoice` / `sendMessage` (answer). Rows go to Postgres.
6. The idle manager keeps the instance warm for the window, pinging its own `/keepalive` through the gateway, then exits.

## Idle manager
Cloud Run only counts requests in progress when deciding an instance is busy; code running in the background does not count. So the manager keeps the instance alive with real requests. Neon has its own clock: its free-plan compute suspends after 5 minutes without queries and that cannot be turned off, so the ping can also keep the database awake.

- **Opens** when any job starts. Never exits while a job or request is running.
- **After a blog is sent:** window ends at now + audio length + first step.
- **After each follow-up answer:** window ends at now + next step. Steps from `app.idle_windows_min`, default `[10, 8, 6, 4, 2]`, floor 2. A new blog resets to the first step.
- **While a Yes/No prompt is pending:** first step.
- **Keep-warm:** while the window is open, `POST /keepalive?key=K` through the gateway every 5 minutes (`app.keepalive_min`). The handler checks the secret header, runs `SELECT 1` when `app.keepalive_db` is true, and returns 204. Turn `keepalive_db` on only if the spike shows the Neon wake-up is noticeable.
- **Hard cap:** the window can never end more than 90 minutes from now (`app.idle_cap_min`), whatever the audio length, so a bug cannot keep an instance up all month.
- **Close:** window over, no job, no request in flight → stop pinging, graceful exit 0. Fallback if Cloud Run reports self-exit as an error: just stop pinging and let Cloud Run remove the idle instance (still free).
- **SIGTERM:** `Core.Close` drain within Cloud Run's grace period. A job still running is lost (resend the link).

## Steps

### A. Spike first (decides whether anything else changes)
1. Neon: create a project and a database role limited to it. Rewrite migrations 001-006 for Postgres (step 4), run them, and run `TestLinksEndToEnd` against a Neon branch and against a local Postgres container. Check every `sql.Tx` path.
2. Neon behaviour: let the database suspend (6+ idle minutes), then time the first query from a local client and from a cold Cloud Run instance. Check that `pgxpool` recovers a connection that was dropped during suspension.
3. Image: build the real image (Go + static ffmpeg on a slim base). Check size (< ~200 MB so two versions fit the 0.5 GB registry free tier). Deploy a throwaway service, let it go cold, time the first request.
4. Read the free-plan numbers on Neon's own pricing page at signup. Its pages disagree (100 vs 50 CU-hours); either is far more than needed.

### B. Go changes
5. **Store:** switch to `pgx/v5` with `pgxpool`, sqlc `sql_package: pgx/v5`, engine `postgresql`. Config `database.url`. Pool: max 1-3 connections (max-instances is 1), `MaxConnIdleTime` ~4 min (under Neon's 5-minute suspend), `MaxConnLifetime` ~30 min, `HealthCheckPeriod` ~1 min. Retry a query once when `pgconn.SafeToRetry(err)` says it is safe. Use Neon's direct (non-pooled) endpoint; with the pooled endpoint, pgx's cached prepared statements need `QueryExecModeCacheDescribe` or the simple protocol (check Neon's docs). Remove `mattn/go-sqlite3`; builds are `CGO_ENABLED=0`.
6. **Migrations:** rewrite 001-006 for Postgres (no production data exists yet, so this is a fresh schema; an optional script can import your local `sevi.db` history). Typical changes: `?` placeholders become `$1`, text datetimes become `timestamptz`, 0/1 integers become `boolean`, ids become `uuid` (still UUIDv7 generated in Go), any SQLite-specific syntax is replaced. If the current migration runner is SQLite-specific, switch to `goose` or `golang-migrate`. Run migrations at boot.
7. **Migration 007:** `seen_updates (update_id unique, created_on)`; `audio_cache.tg_file_id`; confirm `pending` is persisted (the design map says it is; the old deploy plan had it in memory). Migrations convert rows, never drop tables.
8. **Webhook adapter** (`internal/adapter/telegram/`): `http.Handler` for `POST /telegram` doing the checks in step 4 of the flow, then handing the update to the existing per-update goroutine. Keep long polling behind a config flag for local dev. Listen on `$PORT`, set `api.allow_remote_listen`.
9. **file_id reuse:** after `sendAudio`, store the returned `file_id` in `audio_cache`. On a cache hit, send by `file_id`; fall back to re-encoding only if Telegram rejects it.
10. **Idle manager** package wired in `cmd/sevi/main.go` (Core reports job start/end, message kind and audio length). Add `POST /keepalive`.
11. **Secrets:** read one JSON env var (`SEVI_SECRETS`) into config: `telegram_bot_token`, `telegram_webhook_secret`, `openrouter_api_key`, `database_url`, `gateway_api_key` (used by keep-warm pings and the webhook script), `jwt_secret`.
12. **Dev setup:** `docker-compose.yml` with a local Postgres; README explains `make test` against it.

### C. Container
13. `deploy/Dockerfile`: multi-stage, `CGO_ENABLED=0`, slim runtime with a static ffmpeg and the binary. Note that `/tmp` on Cloud Run is memory: a 30-minute blog's raw chunks plus MP3 need ~100-150 MB, hence 1 GiB.

### D. Google Cloud
14. **Kill switch first:** budget at $1 → Pub/Sub topic → small function that disables billing on the project. Budget notices can lag by hours, so max-instances=1 and the idle cap are what limit how fast money could go.
15. **Secret Manager:** one secret `sevi-secrets` (JSON). Rotate with `versions add` and destroy the old version immediately (free tier: 6 active versions).
16. **Artifact Registry:** cleanup policy keeps the last 2 images.
17. **Cloud Run:** `--no-allow-unauthenticated --min-instances=0 --max-instances=1 --no-cpu-throttling --cpu=1 --memory=1Gi --timeout=3600`, secret mounted as `SEVI_SECRETS`. Region must be one API Gateway supports (us-central1 or us-east1).
18. **API Gateway:** service account with only `roles/run.invoker` on the service. `deploy/openapi.yaml` exposes `POST /telegram` and `POST /keepalive`, both with `security: api_key` in the `key` query param, and a backend deadline of ~30 s. Create the API key, restrict it to this API.

### E. Neon
19. Project in the Neon region nearest your Cloud Run region. A dedicated role with rights on this database only. Connection string with `sslmode=require`. Do not add a payment method: the free plan then has nothing to bill. Neon's free plan has a short restore window; if you want more, schedule a weekly `pg_dump` to a free bucket (optional).

### F. OpenRouter
20. Credit limit on the API key. `cost.confirm_over_usd` (0.20) stays.

### G. Register and docs
21. `deploy/set-webhook.sh`: reads the bot token, gateway key and webhook secret from the JSON (never typed into the shell), calls `setWebhook` with `url=https://<gateway>/telegram?key=…` and `secret_token=…`, then `getWebhookInfo`.
22. Update README and `design/implementation.md` Phase 7; add `deploy/` (Dockerfile, openapi.yaml, deploy.sh, set-webhook.sh), `docker-compose.yml` and `design/deploy-architecture.svg`.

## Cost
| Item | Free tier | Expected use / month |
|---|---|---|
| Cloud Run, instance-based | 240,000 vCPU-s, 450,000 GiB-s | ~132,000 vCPU-s worst case (40 × 55 min) |
| API Gateway | 2M calls | ~40 updates + ~400 pings + follow-ups |
| Neon | 100 CU-hours per project (its docs page says 50), 0.5 GB storage, 5 GB transfer | ~10 CU-hours worst case with the `SELECT 1` keep-warm (40 × 55 min × 0.25 CU); text-only data |
| Secret Manager | 6 active versions, 10k accesses | 1 version, ~100 accesses |
| Artifact Registry | 0.5 GB | 2 images |
| Egress (GCP) | 1 GiB within North America | MP3 uploads to Telegram, whose servers are likely outside North America: possibly a few cents. Watch the first bill. |
| OpenRouter | paid | the real cost; capped by the key's credit limit |

## Spam and abuse
- No key: rejected at the gateway, nothing starts.
- Direct `run.app` hit: 403 from IAM, nothing starts.
- Key leaked, no secret header: wakes an instance, Go returns 403. Rotate the key and re-run `set-webhook.sh`.
- Someone else's Telegram account: allowlist, 200 with no work.
- Bot token leaked: the master secret (can replace the webhook and read the chat). Lives only in Secret Manager and local `.env`.
- Database URL leaked: anyone could read your data. It lives only in Secret Manager; rotate the role password in Neon and add a new secret version.
- Backstops: max-instances=1, idle cap, billing kill switch, Neon free-plan limits, OpenRouter credit limit, $0.20 confirm.

## Risks
- Neon wake-up time after suspension is unmeasured. If the first follow-up after a long silence feels slow, turn on `keepalive_db`. Cloud Run's own cold start is also unmeasured; if slow, raise the first window step.
- Neon's free-plan numbers differ between its own pages, and what happens at the 0.5 GB storage cap is not fully clear from what I read (compute suspends at the CU-hour cap; writes may fail at the storage cap). Text-only data should stay far below it; add a storage check to the weekly price job or trim `page_cache` if it grows.
- `pgx`: Debian's tracker lists CVE-2026-41889 against it. Pin the latest release and run `govulncheck` in CI.
- Instance killed mid-job (deploy, rare platform event) loses that job. Deploy only when idle.
- Backups on the free plan are minimal (short restore window only). Optional weekly dump covers it.
- Unverified: gateway-rejected requests start no instance and are not billed; a self-exiting container causes no Cloud Run error; API Gateway accepts the key as `?key=`; Telegram egress cost.

## Verification
1. Spike (step A) passes: migrations on Postgres, `TestLinksEndToEnd` on local Postgres and a Neon branch, stale-connection recovery after suspension, wake-up and cold-start timings, image size.
2. `docker run -p 8080:8080` locally against a dev database. POST fake updates: wrong secret header → 403; non-allowlisted user → 200, no work; repeated `update_id` → 200, no second job; valid update → audio sent, `file_id` stored.
3. Deploy. Gateway with no key and a bad key → rejected, no Cloud Run request logged. `run.app` URL directly → 403, zero instances.
4. `set-webhook.sh`, `getWebhookInfo` shows the URL and no errors. Send a blog link, get the MP3. Send the same link again → instant, $0.00, sent by `file_id`.
5. Idle manager: after a 10-minute blog, instance stays up ~20 minutes with pings in the logs (and Neon stays awake if `keepalive_db` is on), then exits with no Cloud Run error. Follow-ups step the window down. Cap holds at 90 minutes.
6. Scale to zero, then send a follow-up: cold start, history intact from Neon.
7. Kill switch: trigger the function manually on a test project and confirm billing is disabled.