-- name: CreatePending :one
INSERT INTO pending (id, session_id, kind, payload) VALUES (?, ?, ?, ?) RETURNING *;

-- name: GetOpenPending :one
SELECT * FROM pending WHERE session_id = ? AND status = 'pending' ORDER BY created_on DESC, id DESC LIMIT 1;

-- name: SetPendingStatus :exec
UPDATE pending SET status = ? WHERE id = ?;
