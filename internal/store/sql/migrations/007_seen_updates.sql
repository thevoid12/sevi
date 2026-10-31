-- Inbound messages an adapter has already handled (Telegram resends an update it
-- thinks failed), so a repeat is dropped even on a different instance. ref is the
-- adapter's own id for the message (Telegram: update_id).
CREATE TABLE seen_updates (
    id         TEXT PRIMARY KEY,
    adapter    TEXT NOT NULL,
    ref        TEXT NOT NULL,
    created_on TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT,
    updated_on TIMESTAMPTZ,
    updated_by TEXT
);
CREATE INDEX seen_updates_ref ON seen_updates(adapter, ref);
CREATE TRIGGER seen_updates_set_updated_on BEFORE UPDATE ON seen_updates
FOR EACH ROW WHEN (NEW.updated_on IS NOT DISTINCT FROM OLD.updated_on)
EXECUTE FUNCTION set_updated_on();
