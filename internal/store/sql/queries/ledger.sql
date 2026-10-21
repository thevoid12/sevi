-- name: InsertLedger :one
INSERT INTO ledger (id, session_id, role, provider, model, generation_id, chars, input_units, output_units, cost_usd)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: SetLedgerUsage :exec
UPDATE ledger SET input_units = ?, output_units = ?, cost_usd = ? WHERE id = ?;

-- name: ListLedgerBySession :many
SELECT * FROM ledger WHERE session_id = ? ORDER BY created_on, id;

-- name: SessionCost :one
SELECT
    CAST(COALESCE(SUM(cost_usd), 0) AS REAL)             AS total_usd,
    CAST(COUNT(*) AS INTEGER)                            AS calls,
    CAST(COUNT(*) - COUNT(cost_usd) AS INTEGER)          AS unknown_calls
FROM ledger WHERE session_id = ?;
