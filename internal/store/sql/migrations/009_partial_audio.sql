-- Audio of the chunks that were done when a job failed, so sharing the same link
-- again continues from the next chunk. audio_key is raw PCM in the object store.
CREATE TABLE partial_audio (
    id            TEXT PRIMARY KEY,
    url_hash      TEXT   NOT NULL,
    tts_model     TEXT   NOT NULL,
    voice         TEXT   NOT NULL,
    total_chunks  BIGINT NOT NULL,
    done_chunks   BIGINT NOT NULL,
    audio_key     TEXT   NOT NULL,
    cost_usd      DOUBLE PRECISION NOT NULL DEFAULT 0, -- spent on the done chunks, added to the cost when the job resumes
    created_on    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by    TEXT,
    updated_on    TIMESTAMPTZ,
    updated_by    TEXT
);
CREATE INDEX partial_audio_key ON partial_audio(url_hash, tts_model, voice);
CREATE TRIGGER partial_audio_set_updated_on BEFORE UPDATE ON partial_audio
FOR EACH ROW WHEN (NEW.updated_on IS NOT DISTINCT FROM OLD.updated_on)
EXECUTE FUNCTION set_updated_on();
