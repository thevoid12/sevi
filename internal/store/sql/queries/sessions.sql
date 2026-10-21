-- name: CreateSession :one
INSERT INTO sessions (id, kind, url) VALUES (?, ?, ?) RETURNING *;

-- name: GetSession :one
SELECT * FROM sessions WHERE id = ?;
