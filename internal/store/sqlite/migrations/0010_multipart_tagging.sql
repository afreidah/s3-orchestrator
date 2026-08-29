-- Tags supplied on CreateMultipartUpload have to survive until the upload is
-- completed, which may be hours and many parts later, so they are held on the
-- upload row and applied to the object CompleteMultipartUpload produces.
--
-- One column rather than a child table: unlike object_tags, these are only ever
-- read whole for a single upload_id and never filtered by tag, so there is
-- nothing for an index on (tag_key, tag_value) to serve. Aborting or completing
-- an upload drops the row and the tags with it.
--
-- Stored query-string encoded, the same shape the x-amz-tagging header uses.
-- The set is validated at create, so what lands here has already been checked.
--
-- Nullable, and NULL means the upload carried no tags, which every pre-existing
-- row is.

ALTER TABLE multipart_uploads ADD COLUMN tagging TEXT;
