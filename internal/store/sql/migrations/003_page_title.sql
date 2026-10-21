-- The article's own title, taken from the page when it is fetched. NULL for rows cached before this column.
ALTER TABLE page_cache ADD COLUMN title TEXT;
