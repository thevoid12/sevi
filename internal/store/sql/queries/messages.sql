-- name: CreateMessage :one
INSERT INTO messages (id, session_id, role, text, audio_key, parent_id, format)
VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING *;

-- name: GetMessage :one
SELECT * FROM messages WHERE id = ?;

-- name: SetMessageText :exec
UPDATE messages SET text = ? WHERE id = ?;

-- name: ListMessagesBySession :many
SELECT * FROM messages WHERE session_id = ? ORDER BY created_on, id;

-- name: SetMessageAudio :exec
UPDATE messages SET audio_key = ? WHERE id = ?;
