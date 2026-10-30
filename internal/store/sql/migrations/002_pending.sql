-- A question waiting on the user, e.g. "this blog will cost about $0.40, continue?".
-- payload is JSON owned by the code that created the row.
CREATE TABLE pending (
    id         TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    kind       TEXT NOT NULL,
    payload    TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'confirmed', 'cancelled')),
    created_on TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT,
    updated_on TIMESTAMPTZ,
    updated_by TEXT
);
CREATE INDEX pending_session ON pending(session_id);
CREATE TRIGGER pending_set_updated_on BEFORE UPDATE ON pending
FOR EACH ROW WHEN (NEW.updated_on IS NOT DISTINCT FROM OLD.updated_on)
EXECUTE FUNCTION set_updated_on();
