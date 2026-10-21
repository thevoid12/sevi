-- name: CreateMessage :one
INSERT INTO messages (id, session_id, role, text, audio_key) VALUES (?, ?, ?, ?, ?) RETURNING *;

-- name: GetMessage :one
SELECT * FROM messages WHERE id = ?;

-- name: ListMessagesBySession :many
SELECT * FROM messages WHERE session_id = ? ORDER BY created_on, id;
