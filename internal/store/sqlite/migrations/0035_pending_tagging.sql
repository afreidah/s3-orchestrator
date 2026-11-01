-- A write's tags have to survive a failed metadata commit, so the pending
-- reaper can promote the object tagged as the client asked. Without them a
-- promoted object kept the tag set of the object it replaced.
--
-- Stored query-string encoded in one nullable column, the shape the
-- x-amz-tagging header and multipart_uploads.tagging already use. NULL means
-- the write carried no tags, which every pre-existing intent is.

ALTER TABLE pending_objects ADD COLUMN tagging TEXT;
