-- A rolling summary of the older part of a long conversation. summary_through is
-- the id of the last message folded into it; messages after it are sent in full.
ALTER TABLE sessions ADD COLUMN summary TEXT;
ALTER TABLE sessions ADD COLUMN summary_through TEXT;
