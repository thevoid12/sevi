-- name: InsertLedger :one
INSERT INTO ledger (id, session_id, role, provider, model, generation_id, chars, input_units, output_units, cost_usd)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id;

-- name: SetLedgerUsage :exec
UPDATE ledger SET input_units = $1, output_units = $2, cost_usd = $3 WHERE id = $4;

-- name: ListLedgerBySession :many
SELECT * FROM ledger WHERE session_id = $1 ORDER BY created_on, id;

-- name: SessionCost :one
SELECT
    COALESCE(SUM(cost_usd), 0)::float8                  AS total_usd,
    COUNT(*)::bigint                                     AS calls,
    (COUNT(*) - COUNT(cost_usd))::bigint                 AS unknown_calls
FROM ledger WHERE session_id = $1;
