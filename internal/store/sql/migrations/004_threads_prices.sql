-- Conversations: a message remembers which message it replied to and how it was delivered.
ALTER TABLE messages ADD COLUMN parent_id TEXT REFERENCES messages(id);
ALTER TABLE messages ADD COLUMN format TEXT CHECK (format IN ('voice', 'text'));
CREATE INDEX messages_parent ON messages(parent_id);

-- thread_map now covers every message (mine and the bot's), mapped to a stored
-- message; its session is the message's session. Rebuilt because the target changed.
DROP TABLE thread_map;
CREATE TABLE thread_map (
    id         TEXT PRIMARY KEY,
    adapter    TEXT NOT NULL,
    thread_ref TEXT NOT NULL,
    message_id TEXT NOT NULL REFERENCES messages(id),
    created_on INTEGER NOT NULL DEFAULT (unixepoch()),
    created_by TEXT,
    updated_on INTEGER,
    updated_by TEXT,
    UNIQUE (adapter, thread_ref)
);
CREATE TRIGGER thread_map_set_updated_on AFTER UPDATE ON thread_map
FOR EACH ROW WHEN NEW.updated_on IS OLD.updated_on
BEGIN
    UPDATE thread_map SET updated_on = unixepoch() WHERE id = NEW.id;
END;

-- Model prices, USD per token, as last fetched from the provider. Costs are
-- computed as tokens x price instead of asking the provider after every call.
-- fetched_on is when the price was last confirmed; after cost.price_ttl_days
-- it is fetched again.
CREATE TABLE model_prices (
    id             TEXT PRIMARY KEY,
    provider       TEXT NOT NULL,
    model          TEXT NOT NULL,
    prompt_usd     REAL NOT NULL,
    completion_usd REAL NOT NULL,
    audio_usd      REAL NOT NULL DEFAULT 0, -- audio input; 0 = same as prompt
    fetched_on     INTEGER NOT NULL,
    created_on     INTEGER NOT NULL DEFAULT (unixepoch()),
    created_by     TEXT,
    updated_on     INTEGER,
    updated_by     TEXT,
    UNIQUE (provider, model)
);
CREATE TRIGGER model_prices_set_updated_on AFTER UPDATE ON model_prices
FOR EACH ROW WHEN NEW.updated_on IS OLD.updated_on
BEGIN
    UPDATE model_prices SET updated_on = unixepoch() WHERE id = NEW.id;
END;
