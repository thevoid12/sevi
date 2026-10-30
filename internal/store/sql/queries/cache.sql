-- name: GetPage :one
SELECT * FROM page_cache WHERE url_hash = $1 ORDER BY created_on DESC, id DESC LIMIT 1;

-- name: PutPage :exec
WITH upd AS (
    UPDATE page_cache SET text = $5, title = $4 WHERE url_hash = $2 RETURNING id
)
INSERT INTO page_cache (id, url_hash, url, title, text)
SELECT $1, $2, $3, $4, $5 WHERE NOT EXISTS (SELECT 1 FROM upd);

-- name: GetAudio :one
SELECT * FROM audio_cache WHERE url_hash = $1 AND tts_model = $2 AND voice = $3 ORDER BY created_on DESC, id DESC LIMIT 1;

-- name: PutAudio :exec
WITH upd AS (
    UPDATE audio_cache SET audio_key = $5, seconds = $6
    WHERE url_hash = $2 AND tts_model = $3 AND voice = $4 RETURNING id
)
INSERT INTO audio_cache (id, url_hash, tts_model, voice, audio_key, seconds)
SELECT $1, $2, $3, $4, $5, $6 WHERE NOT EXISTS (SELECT 1 FROM upd);
