-- name: CreateMessage :one
INSERT INTO messages (id, session_id, role, text, audio_key, parent_id, format)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *;

-- name: GetMessage :one
SELECT * FROM messages WHERE id = $1;

-- name: SetMessageText :exec
UPDATE messages SET text = $1 WHERE id = $2;

-- name: ListMessagesBySession :many
SELECT * FROM messages WHERE session_id = $1 ORDER BY created_on, id;

-- name: SetMessageAudio :exec
UPDATE messages SET audio_key = $1 WHERE id = $2;
