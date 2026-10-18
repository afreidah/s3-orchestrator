-- Each part upload writes its own path under an intent, so a re-upload of a
-- part number never shares bytes with the part it replaces. storage_key is
-- that path. Parts recorded before it existed keep a NULL and are read from
-- the part number's fixed path.

ALTER TABLE multipart_parts ADD COLUMN storage_key TEXT;
