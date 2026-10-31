-- name: MarkSeen :execrows
INSERT INTO seen_updates (id, adapter, ref)
SELECT $1, $2, $3 WHERE NOT EXISTS (SELECT 1 FROM seen_updates WHERE adapter = $2 AND ref = $3);

-- name: PruneSeen :exec
DELETE FROM seen_updates WHERE created_on < now() - interval '7 days';
