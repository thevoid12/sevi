-- name: GetPartial :one
SELECT * FROM partial_audio WHERE url_hash = $1 AND tts_model = $2 AND voice = $3 ORDER BY created_on DESC, id DESC LIMIT 1;

-- name: PutPartial :exec
WITH upd AS (
    UPDATE partial_audio SET total_chunks = $5, done_chunks = $6, audio_key = $7, cost_usd = $8, created_on = now()
    WHERE url_hash = $2 AND tts_model = $3 AND voice = $4 RETURNING id
)
INSERT INTO partial_audio (id, url_hash, tts_model, voice, total_chunks, done_chunks, audio_key, cost_usd)
SELECT $1, $2, $3, $4, $5, $6, $7, $8 WHERE NOT EXISTS (SELECT 1 FROM upd);

-- name: DeletePartial :exec
DELETE FROM partial_audio WHERE url_hash = $1 AND tts_model = $2 AND voice = $3;
