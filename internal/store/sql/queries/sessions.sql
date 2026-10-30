-- name: CreateSession :one
INSERT INTO sessions (id, kind, url) VALUES ($1, $2, $3) RETURNING *;

-- name: GetSession :one
SELECT * FROM sessions WHERE id = $1;

-- name: SetSessionSummary :exec
UPDATE sessions SET summary = $1, summary_through = $2 WHERE id = $3;
