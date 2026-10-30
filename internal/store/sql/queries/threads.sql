-- name: PutThread :exec
WITH upd AS (
    UPDATE thread_map SET message_id = $4 WHERE adapter = $2 AND thread_ref = $3 RETURNING id
)
INSERT INTO thread_map (id, adapter, thread_ref, message_id)
SELECT $1, $2, $3, $4 WHERE NOT EXISTS (SELECT 1 FROM upd);

-- name: GetThreadMessage :one
SELECT m.* FROM thread_map t JOIN messages m ON m.id = t.message_id
WHERE t.adapter = $1 AND t.thread_ref = $2
ORDER BY t.created_on DESC, t.id DESC LIMIT 1;
