-- One row records when old data was last deleted, so pruning is decided by the
-- database and survives restarts. NULL means never, so the first request prunes.
CREATE TABLE maintenance (
    id             TEXT PRIMARY KEY,
    last_pruned_on TIMESTAMPTZ,
    created_on     TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by     TEXT,
    updated_on     TIMESTAMPTZ,
    updated_by     TEXT
);
INSERT INTO maintenance (id) VALUES ('prune');
