-- Price of cached prompt tokens (providers discount repeated prompt prefixes, e.g.
-- the blog text resent on every turn). 0 = no cache price, bill at the prompt rate.
ALTER TABLE model_prices ADD COLUMN cache_read_usd REAL NOT NULL DEFAULT 0;
-- Prices stored before this column existed lack it; force a refresh on next use.
UPDATE model_prices SET fetched_on = 0;
