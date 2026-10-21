# sevi implementation plan (v1)

Companion to [design.md](design.md). Each phase ends in a **checkpoint**: a
concrete thing you can run and check before the next phase starts. Boxes are
ticked as work completes, so this file is the review surface.

Rules for every phase:
- Everything tunable goes in `config.json`, never hardcoded.
- Logging uses zap's sugared logger; every error is logged with context.
- No secrets in logs or in `config.json` (keys come from env vars named in config).
- Commits stay small, one phase per commit or per sub-step.

Model roles in config (all separately configurable):
- `tts`: text-to-speech only (default `google/gemini-3.8-flash-tts`, from v0).
- `llm`: chat, and audio/file input when `models.stt` is omitted.
- `stt`: optional; present means transcribe first, then call the LLM with text.

---

## Phase 0: Project skeleton

- [x] `go.mod` module path settled; add `design/` to repo, `.gitignore` covers `.env`, `logs/`, `data/`, `output/`
- [x] Directory layout from design.md created (`cmd/sevi`, `internal/...`)
- [x] `config.json` schema defined and `config.example.json` committed (real `config.json` ignored)
- [x] `internal/config`: load, validate, resolve `*_env` key names from environment, fail fast with a clear message
- [x] `internal/logx`: zap sugared logger, console + JSON file (`logs/sevi.log`) + `logs/error.log`, lumberjack rotation from config
- [x] Request id helper carried via `context.Context`

**Checkpoint 0 (done, awaiting review):** `go run ./cmd/sevi` starts, loads config, writes a few log lines to console and `logs/sevi.log`, and a forced error line appears in `logs/error.log`. Missing key env var gives a readable error.

---

## Phase 1: Refactor v0 into packages

- [x] `internal/fetch`: Jina fetch, `toSpeakable`, `detectScript`, `chunk` moved out of `main.go`, unchanged behavior
- [x] `internal/audio`: PCM to MP3 via ffmpeg, plus stitching of chunk audio into one file
- [x] `internal/provider`: `TTS` interface and one implementation over `net/http` (OpenRouter speech endpoint), model/voice/url from config
- [x] TTS retries (`app.retries`, fixed growing backoff; only network errors, 429 and 5xx are retried, a 401 fails at once); a chunk that still fails stops the job with an error naming the chunk, and completed chunks are kept as `<slug>.partial.mp3`
- [x] Per-call usage and cost captured from the provider response, returned to the caller
- [x] Keep a CLI path: `sevi tts <url>` produces `output/<slug>.mp3` and `.cost.md` like v0; the old root `main.go` is removed (in git history)

**Checkpoint 1 (done, awaiting review):** `sevi tts <url>` produces the same quality MP3 as v0 for a blog you already tested, with logs, and prints the cost. A forced bad key yields a clear logged error, not a silent skip.

---

## Phase 2: Store, blob, ledger, cache

- [x] `internal/store`: SQLite (`mattn/go-sqlite3` driver (cgo, needs gcc; first build is slow, later ones are cached)), schema migrations in `internal/store/sql/migrations/` (embedded, applied once each, tracked in `schema_migrations`), tables from design.md
- [x] sqlc: queries in `internal/store/sql/queries/*.sql`, `sqlc.yaml` (dev tool: `go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest`, then `sqlc generate`), generated Go code checked in under `internal/store/db`; no hand-written SQL in Go code
- [x] `internal/blob`: disk audio storage under a configured dir
- [x] Ledger write on every provider call (role, provider, model, units, cost, `unknown_cost` flag)
- [x] Caches: `page_cache` (cleaned text by URL hash) and `audio_cache` (by URL hash + tts model + voice); a re-shared URL costs nothing; `sevi tts -refresh <url>` bypasses both
- [x] Ledger row is written as soon as a provider call succeeds; usage and cost are filled in after the provider's lookup, and stay NULL (unknown) if the lookup fails, for Phase 7 backfill
- [x] Session CRUD and session cost sum
- [x] Table conventions applied to every table: UUID v7 text `id` primary key (generated in Go), `created_on`/`created_by`/`updated_on`/`updated_by`, `updated_on` maintained by a trigger, `*_by` left NULL until accounts exist

**Checkpoint 2 (done, awaiting review):** run `sevi tts <url>` twice. First run writes ledger rows and a cache entry; second run is instant, zero new cost rows. `sqlite3 data/sevi.db` shows sessions and ledger as expected.

---

## Phase 3: Core pipeline and HTTP API (blog sessions)

- [x] `internal/core`: input routing (url, text, audio, file parts), blog session pipeline (fetch, clean, TTS, stitch, store)
- [x] `internal/api`: `POST /v1/messages`, `GET /v1/sessions/:id`, `GET /v1/sessions/:id/cost`, localhost bind from config
- [x] `internal/event`: `Event` type and `UserError{Message, Cause}`; Core emits events, never logs-as-UX; raw causes only go to zap with the request id
- [x] Progress events for long jobs (fetching, chunk i/N, stitching) exposed to adapters (poll or stream); error events carry a `ref` request id
- [x] Rough estimate step: characters to estimated cost and minutes (ledger average, else `cost.est_usd_per_1k_chars`)
- [x] Confirm flow: above `cost.confirm_over_usd`, return an estimate and a pending confirmation; run only after a yes
- [x] Request-id middleware and structured request logging

**Checkpoint 3 (done, awaiting review):** with `curl`, POST a URL: short blog returns audio and cost; a long blog returns an estimate, then confirming runs it. Failure cases (bad URL, blocked fetch, provider error) return a readable message and are in `error.log`.

---

## Phase 4: Telegram adapter (blog to audio)

- [ ] `internal/adapter/telegram`: long polling, config for bot token env var
- [ ] Allowlist: only my Telegram user ID, everyone else is ignored and logged
- [ ] Adapter talks to Core only through the HTTP API
- [ ] Send a URL: status message edited in place with progress, then `sendAudio` with the stitched MP3 and text caption
- [ ] Confirm prompt for long blogs (reply yes, or inline button)
- [ ] Record each sent audio message in `thread_map(adapter, thread_ref, session_id)`
- [ ] Cost footer in the caption (configurable on/off)

**Checkpoint 4 (usable today for the core goal):** share a real blog link in Telegram, watch the status update, receive the audio, play it with a seek bar. Cost shows in the caption. Message from a non-allowlisted account does nothing.

---

## Phase 5: Voice follow-ups on a blog (UC-1 complete)

- [ ] `LLM` interface + implementation using `openai-go` with configurable `base_url`, key, model
- [ ] Audio input path over `net/http` for `models.stt` omitted; confirm the exact request format for the chosen model
- [ ] `models.stt` present: STT interface + implementation, then text to the LLM
- [ ] ffmpeg conversion of Telegram voice notes (OGG/Opus) to the format the provider needs
- [ ] Blog session context: cached blog text plus message history sent each turn
- [ ] System prompt from config: short, conversational, spoken-style, answer in the language of the question; "longer" goes deeper
- [ ] Answer is TTS'd with the `tts` model and returned as audio plus text
- [ ] Telegram: replying to a bot audio message (voice or text) continues that session via `thread_map`
- [ ] Ledger rows for llm, stt, tts per turn; session cost updates

**Checkpoint 5:** play a blog, reply to the audio with a voice note asking a doubt, receive a spoken answer plus text. Reply to that answer with another doubt and it keeps context. Session cost sums all three roles.

---

## Phase 6: Free chat and attachments (UC-2)

- [ ] A voice or text message that is not a reply starts a free chat session
- [ ] Any attached file is passed through to the LLM as session context; provider errors reported plainly
- [ ] Same audio-in, audio-out loop as Phase 5
- [ ] File names and sizes logged, contents never logged

**Checkpoint 6:** send a voice note with a PDF or image attached to the bot with no prior session, get a spoken answer, then continue the conversation by replying.

---

## Phase 7: Hardening

- [ ] Retries and clear user-facing error text for each failure class (fetch blocked, key out of credit, rate limit, ffmpeg missing)
- [ ] Graceful shutdown; in-flight jobs finish or are marked failed
- [ ] Cost backfill for ledger rows flagged `unknown_cost`
- [ ] Log review: no secrets, every error carries session, adapter, operation, model
- [ ] Short `readme.md`: setup, `config.json` reference, running, where logs live

**Checkpoint 7:** a day of real use. Review `logs/error.log` and the ledger for surprises; fix what shows up.

---

## After v1 (not planned yet)

- Web UI over the Core API (sessions, audio players, cost views)
- Discord adapter as the second adapter, to prove the pluggable contract
- **Generic providers (direct keys, no OpenRouter).** Today the config shape (`providers` + model refs) already allows another OpenAI-compatible endpoint, but the code assumes OpenRouter specifics: the TTS call uses OpenRouter's `/audio/speech` Gemini route, cost comes from OpenRouter's usage/generation lookup, and audio input uses OpenRouter's request format. Making it generic means: a `type` per provider (`openrouter`, `openai`, `gemini`, ...) selecting the request/response adapter for TTS, audio input and cost extraction; a fallback cost source when a provider reports none; and no hard dependency on any one provider's key. Revisit once the OpenRouter path works end to end.
- **Playback speed.** User-configurable audio speed (e.g. 1.25x, 1.5x). Applied after synthesis with ffmpeg's `atempo` filter (pitch preserved), so it costs no extra TTS and the cached original stays at normal speed. Config: a default under `models.tts` or a new `audio` group, plus a per-request override later (e.g. a Telegram command). Decide whether to cache sped-up variants or re-encode on delivery.
- Deployment (Cloudflare or otherwise), multi-user BYOK, context summarization
