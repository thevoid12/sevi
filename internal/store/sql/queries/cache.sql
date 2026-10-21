-- name: GetPage :one
SELECT * FROM page_cache WHERE url_hash = ?;

-- name: PutPage :exec
INSERT INTO page_cache (id, url_hash, url, title, text) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (url_hash) DO UPDATE SET text = excluded.text, title = excluded.title;

-- name: GetAudio :one
SELECT * FROM audio_cache WHERE url_hash = ? AND tts_model = ? AND voice = ?;

-- name: PutAudio :exec
INSERT INTO audio_cache (id, url_hash, tts_model, voice, audio_key, seconds) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (url_hash, tts_model, voice)
DO UPDATE SET audio_key = excluded.audio_key, seconds = excluded.seconds;
