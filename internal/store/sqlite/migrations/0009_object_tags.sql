-- S3 object tags: key/value labels attached to an object independently of its
-- data. Stored here rather than on the providers because an object exists as N
-- replicas with no authoritative copy, provider support is inconsistent, and a
-- backend over its usage limit could not be tagged at all.
--
-- One row per tag rather than a JSON column on object_locations. Lifecycle
-- expiry by tag filters on WHERE tag_key = ? AND tag_value = ?, which needs an
-- index; a JSON blob turns that into a scan over every object.
--
-- Keyed by object_key alone, not (object_key, backend_name): tags describe the
-- object, so per-replica rows would let three copies of a key disagree with
-- nothing to say which wins.
--
-- No foreign key, because there is no table to point at. object_locations is
-- keyed (object_key, backend_name) and nothing is keyed on object_key alone.
-- Core clears these rows instead, at every path that puts a new object at a key
-- or removes the last copy of one.
--
-- The primary key already serves lookup and delete by object_key, so only the
-- reverse direction needs its own index.

CREATE TABLE IF NOT EXISTS object_tags (
    object_key TEXT NOT NULL,
    tag_key    TEXT NOT NULL,
    tag_value  TEXT NOT NULL,
    PRIMARY KEY (object_key, tag_key)
);

CREATE INDEX IF NOT EXISTS idx_object_tags_lookup
    ON object_tags(tag_key, tag_value);
