# sevi design (v1 draft)

Status: draft, iterating. Covers [usecase.md](usecase.md) UC-1 and UC-2.

## Goals

- Blog URL in, audio out. Voice follow-up questions, audio answers, as long as needed.
- General voice chat with an LLM, with file attachments (same engine).
- Chat surface is pluggable (Telegram, Discord, ...). The core knows nothing about any of them.
- Web interface is ours, a separate client of the same core.
- Any model provider via bring-your-own-key; models are config, not code.
- Cost tracked per session and per call, ready to show on the web.
- As cheap as possible, ideally near-free on Cloudflare.
- Usable today.

## Architecture

```
 Telegram ─┐
 Discord  ─┼─ adapters ──► Core API ──► Providers (LLM / STT / TTS)
 Web UI   ─┘  (thin)        │  │
                            │  └──► Fetcher (Jina Reader)
                            ▼
                      Store (sessions, messages, costs)
                      Blob  (audio files)
```

Three layers, three separate things:

1. **Core**: sessions, pipeline, providers, cost ledger. Exposes one API.
2. **Adapters**: translate a platform's events to Core API calls and Core
   responses back to platform messages. No business logic. Third parties can
   write their own.
3. **Web UI**: another client of the Core API, plus cost and history views.

## Core API (the adapter contract)

Everything an adapter needs is two operations plus reads. Adapters send
platform-neutral input and get platform-neutral output.

```
POST /v1/messages
  in:  { user, thread_ref, parts: [ {type: url|text|audio|file, ...} ] }
  out: { session_id, parts: [ {type: audio|text, ...} ], cost }

GET  /v1/sessions/:id            history + cost
GET  /v1/sessions/:id/cost       per-call ledger
```

- `thread_ref` is the adapter's own handle for "where this came from", e.g.
  Telegram `chat_id + replied-to message_id`. Core stores a mapping
  `(adapter, thread_ref) -> session_id`. This is how threading works without
  Core knowing what a Telegram reply is.
- Input routing in Core:
  - `url` part with no session: **blog session**. Fetch, clean, TTS, return audio.
  - `audio`/`text` part inside an existing session: **follow-up**. STT if audio,
    LLM with session context, TTS the answer.
  - `audio`/`text` with no session: **free chat session** (UC-2).
  - `file` parts attach to the session as extra context.
- Long jobs (a long blog is many TTS chunks) are async: Core returns a job and
  the adapter gets the result by callback or poll. v1 can hold the request open
  if the adapter tolerates it.

## Sessions and threading

One session = one conversation with a context.

- Blog session context: cleaned blog text + history.
- Free chat context: history + attachments.
- Telegram mapping: every audio message the bot sends is recorded as
  `(chat_id, message_id) -> session_id`. Replying to it, by voice or text,
  continues that session. A message that isn't a reply starts a free chat.
- Web: sessions are explicit (list, open, continue).
- Long context: when history outgrows the model window, summarize older turns.
  Not needed for v1.

## Pipeline

```
blog:      URL -> Jina Reader markdown -> toSpeakable -> chunk -> TTS -> audio
follow-up: voice -> STT -> LLM(context + history) -> answer text -> TTS -> audio
```

- v0 already does the blog path. Reuse its fetch, cleanup and chunking.
- Answers should be written to be spoken: short, no markdown, no tables.
  System prompt enforces it. Also send the text alongside the audio (cheap,
  useful for scanning and copying).
- Gemini can take audio input, so STT and LLM may be one call with an audio-capable
  model. Keep STT as its own interface so it can be split.

## Providers and BYOK

Providers are defined once in `config.json`; each model role points at one
by name and adds only a model ID. Config is grouped by concern (`app`,
`providers`, `models`, `cost`, `telegram`, `log`); see `config.example.json`.

```json
"providers": { "openrouter": { "base_url": "https://openrouter.ai/api/v1", "key_env": "OPENROUTER_API_KEY" } },
"models": {
  "llm": { "provider": "openrouter", "model": "...", "system_prompt": "..." },
  "tts": { "provider": "openrouter", "model": "google/gemini-3.8-flash-tts", "voice": "Kore", "chunk_chars": 3000 }
}
```

- `models.stt` is optional. Absent: the voice note goes straight into one call
  to an audio-capable LLM, which transcribes and answers together. Present:
  transcribe with that model first, then call the LLM with text.
- Default provider is OpenRouter (one key, many models). Any OpenAI-compatible
  endpoint works by changing `base_url`.
- **Client library: no framework. Our own thin `LLM`/`STT`/`TTS` interfaces over
  the OpenAI-compatible HTTP API, using the official `openai/openai-go` SDK for
  chat only.** Reasoning: the only thing we need is "talk to any provider the
  user brings a key for". OpenAI-compatible endpoints are the common
  denominator: OpenRouter exposes hundreds of models behind one, and OpenAI,
  Gemini, Anthropic, Ollama, Groq and others all offer compatible endpoints,
  so `base_url` + key + model covers them. A multi-provider Go library adds
  little on top of that. langchaingo (chains, agents, RAG) is overkill, and a
  web search found no clear industry-standard unified Go client: the space is
  fragmented into small projects (litellm-style clients such as `voocel/litellm`
  and `codewandler/llm`, `lexlapax/go-llms`, `smhanov/llmhub`), none verified
  as widely used. Audio input and TTS are non-standard across providers, so
  those two go over plain `net/http` anyway (v0 already does TTS this way).
  If a provider ever needs a native API, add an implementation of the same
  interface.
- BYOK: keys referenced by env var name in config, never stored in the file,
  never logged. Multi-user encrypted key storage is later.
- Changing a model is a config edit.

## Cost tracking

- Every provider call writes a ledger row:
  `session_id, role (llm|stt|tts), provider, model, input_units, output_units, cost_usd, ts`.
- OpenRouter returns usage and, for chat, cost in the response; v0 already
  collects generation IDs, which can be looked up for exact cost. Where a
  provider returns no cost, leave `cost_usd` null and flag the row `unknown_cost`; cost can be
  backfilled later (e.g. OpenRouter's generation lookup).
- Session cost = sum of its rows. Adapters can show it in a reply footer; web
  shows per-session and per-day totals.
- Cost levers: cheap TTS model, chunk size, skip TTS for very long blogs unless
  confirmed, cache audio by URL hash so re-sharing a link costs nothing.

## Storage

| What | Needs | Local (v1 today) | Cloudflare |
|---|---|---|---|
| Sessions, messages, ledger, thread mapping | relational | SQLite | D1 |
| Audio files, fetched blog text | blobs | disk | R2 |
| Async jobs | queue | in-process | Queues |

SQL is written in `.sql` files and turned into typed Go by sqlc; the SQLite driver is pure-Go `modernc.org/sqlite`. Core talks to `Store` and `Blob` interfaces so the backend is swappable.

## Hosting

Cloudflare is cheap but Workers do not run Go natively (only via limited
WASM). Also, v0 shells out (`os/exec`), likely for audio encoding, and Workers
cannot run ffmpeg. Options:

- **A. Core in TypeScript on Workers.** Native D1/R2/Queues, near-free. Needs a
  rewrite of v0 and an MP3 encoder in JS/WASM or a different audio output
  format. Workers CPU limits need the paid plan ($5/mo) for TTS-sized jobs.
- **B. Core in Go on Cloudflare Containers.** Keeps Go and ffmpeg. Paid plan,
  scales to zero. Containers reach D1/R2 only via a Worker, adding glue.
- **C. Core in Go, run locally now, Cloudflare later.** Zero infra to start,
  testable today. Behind `Store`/`Blob` interfaces, move to B (or A) once the
  shape is proven. Adapters (Telegram via long polling locally, webhook later).

**Decision: C for v1** (confirmed: everything local first, deployment decided after it works). It meets "use it today", keeps the working Go
code, and does not lock in a hosting decision before the API has stabilised.
Cloudflare migration is a later step, and the web UI can be Cloudflare Pages
from the start because it only talks to the Core API.

## v1 scope

In:
- Core in Go with Core API, SQLite, disk blobs.
- Providers behind interfaces, OpenRouter default, models and STT mode in `config.json`.
- Long blogs: all chunks stitched into one audio file.
- Telegram adapter (long polling, allowlisted to my user ID only): link -> audio, reply with voice -> audio
  answer, non-reply voice/text -> free chat, file attach.
- Cost ledger, with a cost footer on replies.

Out (later):
- Discord adapter, web UI, multi-user BYOK, Cloudflare deployment, context
  summarization, async job callbacks.

## Decisions

- Hosting: local first (Go core, SQLite, disk). Deployment later.
- STT: configurable (`models.stt` present or omitted); default (omitted) is one call to an audio-capable model.
- Long blogs: one stitched audio file.
- Delivery: Telegram `sendAudio` (seek bar for long files).
- TTS model, voice, chunk size: in `config.json`, defaults from v0.
- Cost confirm: above a configurable threshold, Core replies with a rough
  cost and duration estimate (from character count and the observed per-character cost) and
  waits for a yes before running TTS.
- Single user. Bot answers only my Telegram user ID.
- LLM client: no framework. Own interfaces over OpenAI-compatible HTTP; `openai-go` for chat, plain `net/http` for audio input and TTS. langchaingo dropped as overkill.
- Default LLM and native-audio input: a Gemini flash model via OpenRouter (exact ID set in `config.json`).
- Adapter link: Core serves the HTTP API on localhost. The Telegram adapter runs in the same binary (goroutine) but only talks to Core through that HTTP API, exactly as an outside adapter would.
- Blog context: the session holds only the blog URL, which is all the adapter sends. Core fetches it with the existing Jina extractor, caches the cleaned text by URL hash (fetched once, reused for follow-ups and for re-shared links), and sends the whole text to the LLM each turn.
- Progress: Telegram shows one status message, edited in place ('fetching', 'audio 3/12'), replaced by the audio when done.
- ffmpeg stays a hard dependency (MP3 encode, chunk stitching, voice-note conversion). Revisit at deployment.
- Language: blog audio is in the blog's language (TTS auto-detects). Follow-up answers match the language of my question.
- Answer style: short, conversational, about 30-60 seconds, no markdown or lists. 'Longer' or 'explain more' goes deeper. Style prompt lives in `config.json`.
- Failures: reply with the reason in one line. Retry a failed TTS chunk a couple of times; if it still fails, say which part failed instead of silently skipping (v0 skipped silently). Partial work is kept.
- Web UI: after Telegram works. Core API and cost ledger are shaped so it needs no core changes.
- Attachments (UC-2): any file type is accepted and passed through to the LLM; the provider/model decides what it can read. Provider size limits apply, and a provider rejection is reported to the user as a plain error.
- Everything tunable lives in `config.json`: models, voice, chunk size, thresholds, prompts, paths, log settings, allowlist, rates.
- Cost: recorded from what the provider returns (OpenRouter reports it). No price table. For the pre-run estimate, use the observed average TTS cost per character from the ledger, falling back to a `cost.est_usd_per_1k_chars` value in config until there is history.
- Logging: Uber `zap`, using the sugared logger (`zap.SugaredLogger`) everywhere. Logs go to stdout and to a rotating file. Errors are always logged with context (session id, adapter, operation, provider, model) and also land in a separate error log file.
- Telegram runs by long polling locally (no public URL).
- Answers are sent as audio plus text (text in the caption or a follow-up message).
- Confirmation threshold: estimated cost over $0.10, configurable (`cost.confirm_over_usd`).

## Cost confirmation flow

```
URL -> fetch + clean -> estimate (chars -> est. TTS cost, est. minutes)
  below threshold:  run immediately
  above threshold:  reply "~12k words, ~$0.04, ~80 min. Go ahead?"  -> yes -> run
```

Estimate = chars x observed cost per char (ledger average, else `cost.est_usd_per_1k_chars`) and chars / spoken-chars-per-minute.
Pending confirmations are stored on the session. Ledger records the actual
cost after the run so estimate quality can be checked.

## Logging

- Library: `go.uber.org/zap`, sugared logger passed into each component
  (`internal/logx` builds it from config). Structured fields via
  `Infow/Errorw`, e.g. `log.Errorw("tts chunk failed", "session", id, "chunk", i, "model", m, "err", err)`.
- Sinks: console (human readable) plus JSON lines to `logs/sevi.log`.
  Errors and above also go to `logs/error.log`.
- Rotation: zap does not rotate files itself, so use `gopkg.in/natefinch/lumberjack.v2`
  as the file writer. Size, backups, age and compression come from config.
- Never log API keys, full audio, or full blog text. Log URL, sizes, durations,
  cost and model.
- Every inbound request gets a request id carried through adapter, core and
  provider calls so one failure can be traced across the log.

```json
"log": { "level": "info", "dir": "logs", "max_size_mb": 20, "max_backups": 10, "max_age_days": 30, "compress": true }
```

## User-facing events vs logs

Two separate channels. Phase 0 only built the second one.

| | Logs (zap) | User events |
|---|---|---|
| Audience | me, debugging | the person in the chat/web |
| Where | console, `sevi.log`, `error.log` | returned by Core API, rendered by the adapter |
| Content | everything: ids, models, stack traces, raw provider errors | only what the user should see, plain language |
| Format | free structured fields | a small typed struct |

Core emits events; it never writes to a chat itself and never knows the surface.

```go
type Event struct {
    Kind    string // progress | notice | error | cost | done
    Message string // safe, human text: "audio 3/12", "Blog fetch was blocked by the site"
    Step, Total int // optional, for progress
    CostUSD *float64 // optional
}
```

- Adapters decide how to show them: Telegram edits one status message for
  `progress`, sends `error` as a short reply, adds `cost` to the caption; the
  web UI shows a progress bar and a cost badge.
- Errors are split in two. Core wraps failures in a `UserError{Message, Cause}`:
  `Cause` (raw provider text, stack) goes only to the logs with the request id;
  `Message` is the only text an adapter sees. Raw provider errors can leak
  details (URLs, quota info, keys in odd cases), so they never reach the user.
- The request id is included in the error event ("ref: 3b09b725") so I can
  grep the logs for exactly that failure.
- Transport: the `POST /v1/messages` response carries the final events;
  long jobs stream or poll events (Phase 3).

## Implementation plan

### Layout

```
cmd/sevi/main.go          wire config, store, core, HTTP server, adapters
internal/config/          config.json + env keys
internal/core/            sessions, routing, pipeline, cost confirm
internal/provider/        LLM / STT / TTS interfaces + openai-go and net/http impls
internal/fetch/           Jina fetch, toSpeakable, chunk (from v0 main.go)
internal/audio/           ffmpeg encode + stitch
internal/store/           SQLite (sessions, messages, ledger, thread map, url cache)
internal/blob/            disk audio storage
internal/api/             HTTP handlers for /v1/*
internal/adapter/telegram/  long polling, calls Core only over HTTP
```

### SQLite tables

- `sessions(id, kind blog|chat, url, created_at)`
- `messages(id, session_id, role user|assistant, text, audio_path, created_at)`
- `thread_map(adapter, thread_ref, session_id)` — Telegram `(chat_id, message_id)` of each bot audio message
- `ledger(id, session_id, role llm|stt|tts, provider, model, input_units, output_units, cost_usd, estimated, ts)`
- `url_cache(url_hash, url, text, audio_path, fetched_at)`
- `pending(session_id, kind confirm, payload, created_at)`

### Build order

1. Refactor v0 into `fetch`, `audio`, `provider/tts`; keep a CLI path that still works.
2. Config + Store + Blob + ledger writes on every provider call.
3. Core pipeline for blog sessions and the HTTP API.
4. Telegram adapter: link to audio, allowlist, status message, `sendAudio`.
5. Cost estimate and confirm flow.
6. Follow-ups: voice note to LLM (native audio) to TTS answer; thread mapping from replies.
7. Free chat and file attachments (UC-2).
8. Hardening: retries, error messages, cost footer.

Steps 1-4 give a usable bot today; 6 delivers UC-1 fully.

## Known limitation (v1)

The provider layer is shaped to be generic (`providers` map, models refer to
them by name) but v1 only implements OpenRouter's request formats for TTS,
audio input and cost reporting. Direct provider keys (OpenAI, Gemini, ...)
need per-provider adapters; tracked under "After v1" in `implementation.md`.

## Open questions

1. **LLM model ID.** v0 contains only a TTS model (`google/gemini-3.8-flash-tts`),
   which cannot chat or hear audio. A separate chat/audio-input model ID is
   needed in `config.json`. To be chosen from OpenRouter's list at step 6.
2. Confirm OpenRouter's audio-input request format for the chosen model (plain HTTP).
