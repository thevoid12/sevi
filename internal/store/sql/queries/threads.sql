-- name: PutThread :exec
INSERT INTO thread_map (id, adapter, thread_ref, message_id) VALUES (?, ?, ?, ?)
ON CONFLICT (adapter, thread_ref) DO UPDATE SET message_id = excluded.message_id;

-- name: GetThreadMessage :one
SELECT m.* FROM thread_map t JOIN messages m ON m.id = t.message_id
WHERE t.adapter = ? AND t.thread_ref = ?;
