# Plan: Deploy sevi at $0 (API Gateway + Cloud Run + Turso)

## Context
Usage is 3 runs a day, 2-3 days a week: about 25-40 sessions a month, roughly 2 hours of compute. Every free tier involved covers that many times over, so cost is not the hard constraint. The design optimises for three things instead: no bill is possible (kill switch everywhere), strangers can never start an instance, and as little new code as possible.

The Go binary (with ffmpeg) runs unchanged in one Cloud Run container. The database moves to Turso, which speaks the same SQLite dialect, so sqlc and migrations 001-006 stay. Telegram reaches the container through Google API Gateway, which checks an API key before anything starts. Cloudflare (D1, R2) is no longer used.

## Decisions (and what they replaced)
| Topic | Decision | Replaced |
|---|---|---|
| Database | Turso over the pure-Go `libsql-client-go` driver. Real transactions, SQLite dialect, no CGO. Free plan, overages off. | D1 over the Cloudflare REST API (custom driver, no interactive transactions) |
| Audio storage | No blob store in production. `audio_cache` stores Telegram's `file_id`; re-sending a blog is a free `sendAudio(file_id)`. Blob interface stays, local disk for dev; object storage comes with the web adapter. | R2 |
| Front door | API Gateway checks `?key=K`; Cloud Run is IAM-only and only the gateway's service account may call it. | (kept) |
| Second lock | Go checks Telegram's `X-Telegram-Bot-Api-Secret-Token` in constant time. | (kept) |
| Background work | Reply 200 to Telegram in milliseconds, run the job in a goroutine. Needs instance-based billing (`--no-cpu-throttling`). | (kept; Cloud Tasks considered and dropped as not worth it for this usage) |
| Instance lifetime | Idle manager keeps the instance warm through the listening + follow-up window with self-pings, then exits. | Idle manager that only shortened lifetime |
| State | Everything that must survive a restart is in Turso: `pending` Yes/No prompts and seen `update_id`s. Running jobs stay in memory (lost on a kill; resend the link, caches make it cheap). | `pending` and dedupe in memory |
| Secrets | One JSON secret in Secret Manager. | Six separate secrets |
| Money | Hard kill switches: GCP budget disables billing, Turso free plan stops at its limit, OpenRouter key has a credit limit. | $1 budget alert (email only) |

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
                                                 ┌── (no self-ping)                ◄──────────┤─ idle manager
                                                 ▼                                         ├─ Core: jobs as goroutines, ffmpeg in /tmp (RAM)
                                            API Gateway                                    │
                                                                                           ├──► api.telegram.org  (sendMessage, editMessageText,
                                                                                           │                       sendAudio / sendVoice, getFile)
                                                                                           ├──► OpenRouter        (chat, TTS, price list)
                                                                                           ├──► Jina Reader       (page → text)
                                                                                           └──► Turso             (all tables, SQL over HTTPS)
Secret Manager (one JSON) · Artifact Registry (image) · Budget → Pub/Sub → billing kill switch
```

## Request flow
1. You send a link or a voice note. Telegram POSTs the update to `https://<gateway>/telegram?key=K` with the `secret_token` header.
2. The gateway checks the key. A missing or wrong key is rejected there and Cloud Run is never contacted. A valid one is forwarded with the gateway service account's token. A request straight to the `run.app` URL gets 403 from Google IAM and starts nothing.
3. If no instance is running, Cloud Run cold-starts one (time to be measured in the spike). No restore step; the database is remote.
4. The webhook handler checks the secret header, caps the body at 64 KB, checks the `from.id` allowlist (200 on miss so Telegram does not retry), records `update_id` in Turso (duplicate → 200, nothing else), then replies 200.
5. A goroutine runs the job. Status goes to Telegram as separate outgoing calls: `sendMessage` once, then `editMessageText` as each step finishes, then `sendAudio` (blog) or `sendVoice` / `sendMessage` (answer). Rows go to Turso.
6. The idle manager keeps the process up for a short window (cold start is ~2 s, so no keep-warm ping), then exits.

## Idle manager
Cloud Run only counts requests in progress when deciding an instance is busy; code running in the background does not count. So the manager keeps the instance alive with real requests.

- **Opens** when any job starts. Never exits while a job or request is running.
- **After a blog is sent:** window ends at now + audio length + first step.
- **After each follow-up answer:** window ends at now + next step. Steps from `app.idle_windows_min`, default `[10, 8, 6, 4, 2]`, floor 2. A new blog resets to the first step.
- **While a Yes/No prompt is pending:** first step.
- **No keep-warm:** cold start measured at ~2 s, so nothing pings the instance. Windows (`idle.*_min`): boot 0.5, after a blog 0.5, first answer 2, each further answer 1 from now, cap 30. Each `exit(0)` is followed by a few restarts from Cloud Run's autoscaler; a ping would only feed that.
- **Hard cap:** the window can never end more than 90 minutes from now (`app.idle_cap_min`), whatever the audio length, so a bug cannot keep an instance up all month.
- **Close:** window over, no job, no request in flight → stop pinging, graceful exit 0. Fallback if Cloud Run reports self-exit as an error: just stop pinging and let Cloud Run remove the idle instance (still free).
- **SIGTERM:** `Core.Close` drain within Cloud Run's grace period. A job still running is lost (resend the link).

## Steps

### A. Spike first (decides nothing else changes)
1. Turso: create a database, run migrations 001-006 through `libsql-client-go`, run `TestLinksEndToEnd` against it. Check `sql.Tx` paths in `internal/store` and `internal/core` work over the wire. Test a stale pooled connection after 20 idle minutes (a known weak spot of that driver; a maintained fork exists if upstream misbehaves).
2. Image: build the real image (Go + static ffmpeg on a slim base). Check size (< ~200 MB so two versions fit the 0.5 GB registry free tier). Deploy a throwaway service, let it go cold, time the first request and the first Turso query.

### B. Go changes
3. **Store:** choose the driver by config. Local dev and tests keep file SQLite; production uses `libsql://<db>.turso.io?authToken=…`. Migrations run at boot through the same `database/sql` handle.
4. **Migration 007:** `seen_updates (update_id unique, created_on)`; `audio_cache.tg_file_id`; confirm `pending` is persisted (the design map says it is; the old deploy plan had it in memory). New migrations convert rows, never drop tables.
5. **Webhook adapter** (`internal/adapter/telegram/`): `http.Handler` for `POST /telegram` doing the checks in step 4 of the flow, then handing the update to the existing per-update goroutine. Keep long polling behind a config flag for local dev. Listen on `$PORT`, set `api.allow_remote_listen`.
6. **file_id reuse:** after `sendAudio`, store the returned `file_id` in `audio_cache`. On a cache hit, send by `file_id`; fall back to re-encoding only if Telegram rejects it.
7. **Idle manager** package wired in `cmd/sevi/main.go` (Core reports job start/end, message kind and audio length).
8. **Secrets:** read one JSON env var (`SEVI_SECRETS`) into config: `telegram_bot_token`, `telegram_webhook_secret`, `openrouter_api_key`, `turso_url`, `turso_token`, `gateway_api_key` (used by the webhook script), `jwt_secret`.

### C. Container
9. `deploy/Dockerfile`: multi-stage, `CGO_ENABLED=0`, slim runtime with a static ffmpeg and the binary. Note that `/tmp` on Cloud Run is memory: a 30-minute blog's raw chunks plus MP3 need ~100-150 MB, hence 1 GiB.

### D. Google Cloud
10. **Kill switch first:** budget at $1 → Pub/Sub topic → small function that disables billing on the project. Budget notices can lag by hours, so max-instances=1 and the idle cap are what limit how fast money could go.
11. **Secret Manager:** one secret `sevi-secrets` (JSON). Rotate with `versions add` and destroy the old version immediately (free tier: 6 active versions).
12. **Artifact Registry:** cleanup policy keeps the last 2 images.
13. **Cloud Run:** `--no-allow-unauthenticated --min-instances=0 --max-instances=1 --no-cpu-throttling --cpu=1 --memory=1Gi --timeout=3600`, secret mounted as `SEVI_SECRETS`. Region must be one API Gateway supports (us-central1 or us-east1).
14. **API Gateway:** service account with only `roles/run.invoker` on the service. `deploy/openapi.yaml` exposes `POST /telegram`, with `security: api_key` in the `key` query param, and a backend deadline of ~30 s. Create the API key, restrict it to this API.

### E. Turso
15. Database in the region nearest the Cloud Run region. Database-scoped auth token. Confirm the account has overages off.

### F. OpenRouter
16. Credit limit on the API key. `cost.confirm_over_usd` (0.20) stays.

### G. Register and docs
17. `deploy/set-webhook.sh`: reads the bot token, gateway key and webhook secret from the JSON (never typed into the shell), calls `setWebhook` with `url=https://<gateway>/telegram?key=…` and `secret_token=…`, then `getWebhookInfo`.
18. Update README and `design/implementation.md` Phase 7; add `deploy/` (Dockerfile, openapi.yaml, deploy.sh, set-webhook.sh) and `design/deploy-architecture.svg`.

## Cost
| Item | Free tier | Expected use / month |
|---|---|---|
| Cloud Run, instance-based | 240,000 vCPU-s, 450,000 GiB-s | ~132,000 vCPU-s worst case (40 × 55 min) |
| API Gateway | 2M calls | ~40 updates + ~400 pings + follow-ups |
| Turso | 5 GB, 500M row reads, 10M row writes | tiny |
| Secret Manager | 6 active versions, 10k accesses | 1 version, ~100 accesses |
| Artifact Registry | 0.5 GB | 2 images |
| Egress | 1 GiB within North America | MP3 uploads to Telegram, whose servers are likely outside North America: possibly a few cents. Watch the first bill. |
| OpenRouter | paid | the real cost; capped by the key's credit limit |

## Spam and abuse
- No key: rejected at the gateway, nothing starts.
- Direct `run.app` hit: 403 from IAM, nothing starts.
- Key leaked, no secret header: wakes an instance, Go returns 403. Rotate the key and re-run `set-webhook.sh`.
- Someone else's Telegram account: allowlist, 200 with no work.
- Bot token leaked: the master secret (can replace the webhook and read the chat). Lives only in Secret Manager and local `.env`.
- Backstops: max-instances=1, idle cap, billing kill switch, Turso free plan limits, OpenRouter credit limit, $0.20 confirm.

## Risks
- `libsql-client-go` upkeep: Turso's docs recommend it for remote access, but a community fork reports upstream deprecation and pooled-connection bugs. The spike tests stale connections; switch to the fork if needed.
- Cold start time is unmeasured. If the first follow-up after a long silence feels slow, raise the first window step.
- Instance killed mid-job (deploy, rare platform event) loses that job. Deploy only when idle.
- Unverified: gateway-rejected requests start no instance and are not billed; a self-exiting container causes no Cloud Run error; API Gateway accepts the key as `?key=`; Telegram egress cost.

## Verification
1. Spike (step A) passes: migrations, `TestLinksEndToEnd` on Turso, stale-connection test, image size, cold start timed.
2. `docker run -p 8080:8080` locally with a dev Turso DB. POST fake updates: wrong secret header → 403; non-allowlisted user → 200, no work; repeated `update_id` → 200, no second job; valid update → audio sent, `file_id` stored.
3. Deploy. Gateway with no key and a bad key → rejected, no Cloud Run request logged. `run.app` URL directly → 403, zero instances.
4. `set-webhook.sh`, `getWebhookInfo` shows the URL and no errors. Send a blog link, get the MP3. Send the same link again → instant, $0.00, sent by `file_id`.
5. Idle manager: after a blog the instance exits within ~30 s; follow-ups keep it 1-2 min. Cap holds at 30 minutes.
6. Scale to zero, then send a follow-up: cold start, history intact from Turso.
7. Kill switch: trigger the function manually on a test project and confirm billing is disabled.