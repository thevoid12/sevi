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
    created_on TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT,
    updated_on TIMESTAMPTZ,
    updated_by TEXT
);
CREATE INDEX thread_map_ref ON thread_map(adapter, thread_ref);
CREATE TRIGGER thread_map_set_updated_on BEFORE UPDATE ON thread_map
FOR EACH ROW WHEN (NEW.updated_on IS NOT DISTINCT FROM OLD.updated_on)
EXECUTE FUNCTION set_updated_on();

-- Model prices, USD per token, as last fetched from the provider. Costs are
-- computed as tokens x price instead of asking the provider after every call.
-- fetched_on is when the price was last confirmed; after cost.price_ttl_days
-- it is fetched again.
CREATE TABLE model_prices (
    id             TEXT PRIMARY KEY,
    provider       TEXT NOT NULL,
    model          TEXT NOT NULL,
    prompt_usd     DOUBLE PRECISION NOT NULL,
    completion_usd DOUBLE PRECISION NOT NULL,
    audio_usd      DOUBLE PRECISION NOT NULL DEFAULT 0, -- audio input; 0 = same as prompt
    fetched_on     BIGINT NOT NULL,
    created_on     TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by     TEXT,
    updated_on     TIMESTAMPTZ,
    updated_by     TEXT
);
CREATE INDEX model_prices_key ON model_prices(provider, model);
CREATE TRIGGER model_prices_set_updated_on BEFORE UPDATE ON model_prices
FOR EACH ROW WHEN (NEW.updated_on IS NOT DISTINCT FROM OLD.updated_on)
EXECUTE FUNCTION set_updated_on();
