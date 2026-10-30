-- name: GetPrice :one
SELECT * FROM model_prices WHERE provider = $1 AND model = $2 ORDER BY created_on DESC, id DESC LIMIT 1;

-- name: ListPricedModels :many
SELECT model FROM model_prices WHERE provider = $1;

-- name: UpsertPrice :exec
WITH upd AS (
    UPDATE model_prices SET prompt_usd = $4, completion_usd = $5, audio_usd = $6, cache_read_usd = $7, fetched_on = $8
    WHERE provider = $2 AND model = $3 RETURNING id
)
INSERT INTO model_prices (id, provider, model, prompt_usd, completion_usd, audio_usd, cache_read_usd, fetched_on)
SELECT $1, $2, $3, $4, $5, $6, $7, $8 WHERE NOT EXISTS (SELECT 1 FROM upd);

-- name: LatestCostedLedger :one
SELECT * FROM ledger
WHERE provider = $1 AND model = $2 AND generation_id IS NOT NULL AND cost_usd IS NOT NULL
ORDER BY created_on DESC, id DESC LIMIT 1;
