-- name: PruneOld :exec
-- One statement, so one transaction: if the last prune was more than $1 days ago,
-- delete everything older than $1 days and record the time; otherwise do nothing.
-- Two requests at once cannot both prune: the second waits on the row lock, then
-- sees the new time and finds nothing due.
-- A session is old when its newest activity (its own row or any message) is old;
-- its messages, thread links, pending items and ledger rows go with it.
-- Caches, partial audio and seen ids are old by their own age. Prices have their own refresh.
WITH due AS (
    UPDATE maintenance SET last_pruned_on = now()
    WHERE id = 'prune'
      AND (last_pruned_on IS NULL OR last_pruned_on < now() - ($1::int * interval '1 day'))
    RETURNING id
),
old AS (
    SELECT s.id FROM sessions s
    WHERE EXISTS (SELECT 1 FROM due)
      AND GREATEST(
              s.created_on,
              COALESCE(s.updated_on, s.created_on),
              COALESCE((SELECT max(m.created_on) FROM messages m WHERE m.session_id = s.id), s.created_on)
          ) < now() - ($1::int * interval '1 day')
),
d_threads AS (
    DELETE FROM thread_map WHERE message_id IN (SELECT id FROM messages WHERE session_id IN (SELECT id FROM old))
),
d_pending AS (DELETE FROM pending WHERE session_id IN (SELECT id FROM old)),
d_ledger AS (DELETE FROM ledger WHERE session_id IN (SELECT id FROM old)),
d_messages AS (DELETE FROM messages WHERE session_id IN (SELECT id FROM old)),
d_pages AS (
    DELETE FROM page_cache
    WHERE EXISTS (SELECT 1 FROM due) AND COALESCE(updated_on, created_on) < now() - ($1::int * interval '1 day')
),
d_audio AS (
    DELETE FROM audio_cache
    WHERE EXISTS (SELECT 1 FROM due) AND created_on < now() - ($1::int * interval '1 day')
),
d_partial AS (
    DELETE FROM partial_audio
    WHERE EXISTS (SELECT 1 FROM due) AND created_on < now() - ($1::int * interval '1 day')
),
d_seen AS (
    DELETE FROM seen_updates
    WHERE EXISTS (SELECT 1 FROM due) AND created_on < now() - ($1::int * interval '1 day')
)
DELETE FROM sessions WHERE id IN (SELECT id FROM old);
