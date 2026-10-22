# sevi design (version 0)
check [design](sevi-design-map.html) for the design of this version
Status: version 0 is built and running; this file is revised as it is reviewed. Covers [usecase.md](usecase.md) UC-1 and, in version 1, UC-2's attachments.

Naming: **the prototype** is the original single-file CLI; **version 0** is the current working app (link to audio, voice and text follow-ups); **version 1** is the next round (attachments, hardening, readme, and the items under Version 1 in [implementation.md](implementation.md)).

## Goals

- Blog URL in, audio out. Voice follow-up questions, audio answers, as long as needed.
- General voice chat with an LLM (version 0). File attachments on top of it (version 1).
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
- Messages start **jobs**. A job is one unit of work; adapters follow it:

```
POST /v1/messages            -> 202 {job_id, session_id, status, events}
POST /v1/messages?wait=1     -> blocks until the job stops running, same shape + parts
GET  /v1/jobs/{id}?after=N&wait=S   events with seq > N; long-polls up to S seconds
GET  /v1/audio/{message_id}  the MP3 (range requests, so seeking works)
```

  Job status: `running`, `done`, `failed`, `needs_confirmation`. A job is
  `done` as soon as the audio is ready; its cost arrives later as a `cost`
  event (`cost_pending` is true until then), because the provider's usage
  record can lag by up to a minute. Jobs are held in memory (lost on restart);
  sessions, messages and the ledger are in SQLite.
- Input parts: `url` (a text part that is just a link also counts), and
  `confirm` with `decision: yes|no` plus the `session_id` being answered.
  Output parts: `audio` (`message_id`, `seconds`) and `text`.
- Input routing in Core:
  - `url` part: a new **blog session**. Fetch, clean, estimate, TTS, return audio.
  - Estimated cost over `cost.confirm_over_usd`: the job ends `needs_confirmation`
    with a `confirm` event ("about 8.7k characters, roughly 9 minutes, around
    $0.13. Go ahead?") and a row in `pending`. A `confirm` message resumes or
    cancels it.
  - `audio`/`text` inside an existing session (follow-ups) and free chat: Phase 5-6.
- Errors over HTTP: `{"error": {"message": <safe text>, "ref": <request id>}}`.
  Every response carries `X-Request-Id`; a caller may supply one.
- The API binds to `app.listen` (localhost) with no authentication in version 0.

## Sessions and threading

A session is one conversation: a blog and every question asked about it, or a
free chat. Every message is stored with its time (`created_on`), its role, its
text (the transcript for voice) and a `parent_id`: the message it was a reply to.

- **Context for any turn = the whole session in time order**, each message with
  its timestamp, plus the blog text for a blog session. It does not matter
  which message I replied to: all questions about a blog form a single thread,
  so a question asked from an old message still sees everything said since.
- The message I replied to is marked in the prompt ("the user is replying to
  this message: ..."), so the model knows what "that" or "the second point"
  refers to.
- `parent_id` is kept for that marker and for a web UI that shows the reply
  structure; it does not limit what the model sees.
- Transcripts are stored, so later turns send text, not old audio again. The
  user's own voice notes are also kept as audio for replay.
- **Thread map.** `thread_map` maps an adapter's handle for *every* message,
  both mine and the bot's (Telegram `chat_id:message_id`), to a message id, so
  a reply to any of them finds its session.
  - Core registers my message itself when it arrives (it knows my handle and
    which handle I replied to).
  - The adapter registers the bot's messages after sending them, through
    `POST /v1/threads {adapter, thread_ref, message_id}`.
  - A message that is not a reply starts a new free chat with no blog. A reply
    to a message sevi does not know (e.g. an audio sent before threading was
    tracked) is **refused with an explanation**, not answered in a blank chat:
    the person meant a particular blog, and an answer without it would be a
    guess presented as a reply. (This happened once: the model, given no blog,
    invented one.)
  - A free chat tells the model plainly that it has no blog or document and
    must not invent one; the system prompt also says to use only the material
    given and to say so when the answer is not in it.
- Web: sessions are explicit (list, open, continue).
- Long context: when a session outgrows the model window (1M tokens on the
  chosen model, so far away), summarize older turns. Not needed for version 0.

## Reply format (voice or text)

Config: `reply.voice` (default `true`). One LLM call returns structured JSON:

```json
{ "transcript": "what the user said (if audio)", "format": "voice | text", "answer": "..." }
```

- The system prompt tells the model the default format (from `reply.voice`) and
  to override it for this turn only when the NEW message explicitly asks for the
  other format; a request in an earlier message does not carry over (without
  this rule a model treated one "answer as text" as a standing preference). The model decides, so it works in any language and phrasing.
- `reply.voice = false`: default text, and the same rule in reverse: an explicit
  request for audio gets a voice note for that turn.
- `format = voice`: the answer is spoken with the TTS model and sent as audio;
  `format = text`: sent as a text message, and no TTS cost is incurred.
- The blog link flow is unaffected: a link always produces audio.

## Pipeline

```
blog:      URL -> Jina Reader markdown -> toSpeakable -> chunk -> TTS -> audio
follow-up: voice -> LLM(whole session in time order + blog text, hears the audio) -> {transcript, format, answer}
           format=voice: answer -> TTS -> audio     format=text: answer as text
```

- The prototype already did the blog path. Its fetch, cleanup and chunking were reused.
- Answers should be written to be spoken: short, no markdown, no tables.
  System prompt enforces it. For text answers the same brevity applies unless
  asked otherwise.
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
  those two go over plain `net/http` anyway (the prototype already did TTS this way).
  If a provider ever needs a native API, add an implementation of the same
  interface.
- BYOK: keys referenced by env var name in config, never stored in the file,
  never logged. Multi-user encrypted key storage is later.
- Changing a model is a config edit.

## Model call retries

Chat and transcription calls go through `provider.Retrying`. A 429 (shared
upstream pools such as Google AI Studio behind OpenRouter rate-limit in bursts of
several seconds), a 5xx or a network error is retried with growing waits
(3s, 6s, 12s, 24s; `app.retries` extra attempts, 4 in the example config). A
rejected key, an out-of-credit account or a bad request is not retried. While
waiting, the job emits a progress event so the person sees "trying again in N
seconds" instead of silence. The speech client retries the same way.

## Cost tracking

Costs are **computed, not looked up**: tokens x a stored per-token price. There
is no provider call per generation.

- **Prices** live in the `model_prices` table (prompt, completion, audio input,
  cache-read; USD per token) with the time they were fetched. They come from
  one public call that lists every model (OpenRouter `GET /models?output_modalities=all`,
  which includes speech models). A price is trusted for `cost.price_ttl_days`
  (default 7). When one is older, the next cost calculation refetches the list,
  re-prices every stored model in that one call, and logs any price that changed.
  If the refetch fails, the stored price is used and the failure is logged.
- **Tokens** come from the response for chat and transcription (`usage`, including
  the audio-token and cached-token parts). A speech call returns no usage, so its
  tokens are derived: output = seconds of audio produced x 25 (Gemini TTS bills
  25 per second; measured 25.0 on real runs), input = characters / 4.5.
- **Cached prompt tokens.** Providers discount a repeated prompt prefix (the blog
  text resent every turn); those tokens are priced at the cache-read rate.
  The usage report does not say whether cached tokens were text or audio, so a
  turn that cached audio is priced a little high (under $0.0001 in practice).
- **Ledger.** Every provider call is a `ledger` row written as it happens, with
  tokens and cost, so a job's cost is known the moment it finishes. A row whose
  price could not be found keeps a NULL cost and is counted as "unpriced".
- **Check against reality.** On each price refresh, for each model, the latest
  ledger row is compared with what the provider says it charged (one lookup per
  model per refresh, in the background). A gap over 10% and over a tenth of a
  cent is logged as a warning; otherwise it is logged as a match.
- Measured on real calls: speech within 0.1% of the provider's charge, chat
  exact for uncached turns.
- **Estimate before a long job** = the stored speech price x tokens implied by the
  character count and the speaking rate. Real example: 8,722 characters estimated
  $0.125, billed $0.121. If no price is available the fallback is
  `cost.est_usd_per_1k_chars`.
- Cost levers: cheap TTS model, text answers (no speech), the audio cache
  (re-shared links cost nothing), short answers by prompt.

## Storage

| What | Needs | Local (today) | Cloudflare |
|---|---|---|---|
| Sessions, messages, ledger, thread mapping | relational | SQLite | D1 |
| Audio files, fetched blog text | blobs | disk | R2 |
| Async jobs | queue | in-process | Queues |

SQL is written in `.sql` files and turned into typed Go by sqlc; the SQLite driver is `mattn/go-sqlite3` (cgo; needs gcc to build, which matters for the deployment step). Core talks to `Store` and `Blob` interfaces so the backend is swappable.

## Hosting

Cloudflare is cheap but Workers do not run Go natively (only via limited
WASM). Also, the prototype shells out (`os/exec`), likely for audio encoding, and Workers
cannot run ffmpeg. Options:

- **A. Core in TypeScript on Workers.** Native D1/R2/Queues, near-free. Needs a
  rewrite of the Go code and an MP3 encoder in JS/WASM or a different audio output
  format. Workers CPU limits need the paid plan ($5/mo) for TTS-sized jobs.
- **B. Core in Go on Cloudflare Containers.** Keeps Go and ffmpeg. Paid plan,
  scales to zero. Containers reach D1/R2 only via a Worker, adding glue.
- **C. Core in Go, run locally now, Cloudflare later.** Zero infra to start,
  testable today. Behind `Store`/`Blob` interfaces, move to B (or A) once the
  shape is proven. Adapters (Telegram via long polling locally, webhook later).

**Decision: C for version 0** (confirmed: everything local first, deployment decided after it works). It meets "use it today", keeps the working Go
code, and does not lock in a hosting decision before the API has stabilised.
Cloudflare migration is a later step, and the web UI can be Cloudflare Pages
from the start because it only talks to the Core API.

## Version 0 scope

In (built):
- Core in Go with Core API, SQLite, disk blobs.
- Providers behind interfaces, OpenRouter default, models and STT mode in `config.json`.
- Long blogs: all chunks stitched into one audio file; Yes/No before anything over `cost.confirm_over_usd`.
- Telegram adapter (long polling, allowlisted to my user ID only): link -> audio;
  reply with voice or text -> answer as a voice note or text; a message that is not a reply -> free chat.
- Cost computed from tokens and stored prices, shown on every answer.

Version 1 (not in version 0):
- File attachments (documents, images) as context, in any chat.
- Hardening: a clear error text per failure class, graceful shutdown, ledger backfill, log review.
- A readme. Discord adapter, web UI, multi-user BYOK, generic providers, playback speed,
  context summarization, resumable jobs.

Between the two: fixes found in the design review, then deployment (Cloudflare).

## Decisions

- Hosting: local first (Go core, SQLite, disk). Deployment later.
- STT: configurable (`models.stt` present or omitted); default (omitted) is one call to an audio-capable model.
- Long blogs: one stitched audio file.
- Delivery: Telegram `sendAudio` (seek bar for long files).
- TTS model, voice, chunk size: in `config.json`, defaults from the prototype.
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
- Failures: reply with the reason in one line. Retry a failed TTS chunk a couple of times; if it still fails, say which part failed instead of silently skipping (the prototype skipped silently). Partial work is kept.
- Web UI: after Telegram works. Core API and cost ledger are shaped so it needs no core changes.
- Attachments (UC-2): any file type is accepted and passed through to the LLM; the provider/model decides what it can read. Provider size limits apply, and a provider rejection is reported to the user as a plain error.
- Everything tunable lives in `config.json`: models, voice, chunk size, thresholds, prompts, paths, log settings, allowlist, rates.
- Cost: computed as tokens x a stored per-token price, with the price refetched at most every `cost.price_ttl_days` (7) and checked against one real charge per refresh. No per-call provider lookup.
- Logging: Uber `zap`, using the sugared logger (`zap.SugaredLogger`) everywhere. Logs go to stdout and to a rotating file. Errors are always logged with context (session id, adapter, operation, provider, model) and also land in a separate error log file.
- LLM model: `google/gemini-3.5-flash-lite` (USD per 1M tokens: text in 0.30, out 2.50, audio in 0.30, cached in 0.03). Chosen over the cheaper `gemini-2.5-flash-lite` (0.10 / 0.40), which was rate-limited upstream far too often on OpenRouter (2 of 11 calls); the 3.5 model's costs matched the provider's charge to the digit. A turn costs about $0.001 in LLM, against about $0.003 to speak the answer. Newer non-lite Gemini models (3.6 to 3.8) cost 2.5 to 3 times more. Swapping is a one-line config change. The `:batch` variants are asynchronous and not usable for chat.
- One thread per blog: the model sees the whole session in time order with timestamps whichever message is replied to; the replied-to message is marked in the prompt. A message that is not a reply starts a new free chat; a reply to an unknown message is refused with an explanation.
- Spoken answers go out as Telegram voice notes (OGG/Opus, native waveform), voice only with no text caption; the blog audio stays a normal audio file.
- Reply format: `reply.voice` in config (default true); the model overrides it per turn when the user explicitly asks for text.
- Telegram runs by long polling locally (no public URL).
- Answers are sent as audio plus text (text in the caption or a follow-up message).
- Confirmation threshold: estimated cost over $0.20, configurable (`cost.confirm_over_usd`).

## Cost confirmation flow

```
URL -> fetch + clean -> estimate (chars -> est. TTS cost, est. minutes)
  below threshold:  run immediately
  above threshold:  reply "~12k words, ~$0.04, ~80 min. Go ahead?"  -> yes -> run
```

Estimate = tokens implied by the character count and speaking rate x the stored speech price (fallback `cost.est_usd_per_1k_chars`); minutes = chars / spoken-chars-per-minute.
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
internal/fetch/           Jina fetch, toSpeakable, chunk (from the prototype's main.go)
internal/audio/           ffmpeg encode + stitch
internal/store/           SQLite (sessions, messages, ledger, thread map, url cache)
internal/blob/            disk audio storage
internal/api/             HTTP handlers for /v1/*
internal/adapter/telegram/  long polling, calls Core only over HTTP
```

### SQLite tables

- `sessions(id, kind blog|chat, url, created_at)`
- `messages(id, session_id, role user|assistant, text, audio_path, created_at)`
- `thread_map(adapter, thread_ref, message_id)` — Telegram `chat_id:message_id` of every message, mine and the bot's, mapped to a stored message
- `ledger(id, session_id, role llm|stt|tts, provider, model, input_units, output_units, cost_usd, estimated, ts)`
- `page_cache(url_hash, url, text, fetched_at)` and `audio_cache(url_hash, tts_model, voice, audio_key, seconds)`
- `pending(session_id, kind confirm, payload, created_at)`

### Build order

1. Refactor the prototype into `fetch`, `audio`, `provider/tts`; keep a CLI path that still works.
2. Config + Store + Blob + ledger writes on every provider call.
3. Core pipeline for blog sessions and the HTTP API.
4. Telegram adapter: link to audio, allowlist, status message, `sendAudio`.
5. Cost estimate and confirm flow.
6. Follow-ups: voice note to LLM (native audio) to TTS answer; thread mapping from replies.
7. Free chat (built in step 6). File attachments: version 1.
8. Hardening: version 1 (retries on rate limits were already done in version 0).

Steps 1-4 give a usable bot today; 6 delivers UC-1 fully.

## Known limitation (version 0)

The provider layer is shaped to be generic (`providers` map, models refer to
them by name) but version 0 only implements OpenRouter's request formats for TTS,
audio input and cost reporting. Direct provider keys (OpenAI, Gemini, ...)
need per-provider adapters; tracked under Version 1 in `implementation.md`.

## Open questions

1. **LLM model ID.** The prototype contained only a TTS model (`google/gemini-3.8-flash-tts`),
   which cannot chat or hear audio. A separate chat/audio-input model ID is
   needed in `config.json`. To be chosen from OpenRouter's list at step 6.
2. Confirm OpenRouter's audio-input request format for the chosen model (plain HTTP).
