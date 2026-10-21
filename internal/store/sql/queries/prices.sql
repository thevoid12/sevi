-- name: GetPrice :one
SELECT * FROM model_prices WHERE provider = ? AND model = ?;

-- name: ListPricedModels :many
SELECT model FROM model_prices WHERE provider = ?;

-- name: UpsertPrice :exec
INSERT INTO model_prices (id, provider, model, prompt_usd, completion_usd, audio_usd, cache_read_usd, fetched_on)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (provider, model) DO UPDATE SET
    prompt_usd = excluded.prompt_usd, completion_usd = excluded.completion_usd,
    audio_usd = excluded.audio_usd, cache_read_usd = excluded.cache_read_usd, fetched_on = excluded.fetched_on;

-- name: LatestCostedLedger :one
SELECT * FROM ledger
WHERE provider = ? AND model = ? AND generation_id IS NOT NULL AND cost_usd IS NOT NULL
ORDER BY created_on DESC, id DESC LIMIT 1;
