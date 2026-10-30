-- name: CreatePending :one
INSERT INTO pending (id, session_id, kind, payload) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetOpenPending :one
SELECT * FROM pending WHERE session_id = $1 AND status = 'pending' ORDER BY created_on DESC, id DESC LIMIT 1;

-- name: SetPendingStatus :exec
UPDATE pending SET status = $1 WHERE id = $2;
