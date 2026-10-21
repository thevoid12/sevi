-- name: PutThread :exec
INSERT INTO thread_map (id, adapter, thread_ref, session_id) VALUES (?, ?, ?, ?)
ON CONFLICT (adapter, thread_ref) DO UPDATE SET session_id = excluded.session_id;

-- name: GetThread :one
SELECT * FROM thread_map WHERE adapter = ? AND thread_ref = ?;
