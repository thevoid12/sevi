-- App-wide settings chosen at run time (for now: the voice and accent), as key/value text.
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    created_on TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT,
    updated_on TIMESTAMPTZ,
    updated_by TEXT
);
CREATE TRIGGER settings_set_updated_on BEFORE UPDATE ON settings
FOR EACH ROW WHEN (NEW.updated_on IS NOT DISTINCT FROM OLD.updated_on)
EXECUTE FUNCTION set_updated_on();
