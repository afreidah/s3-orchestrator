-- Pad every stored timestamp to a fixed nine-digit fractional part.
--
-- SQLite holds these columns as TEXT, so ORDER BY and every range comparison
-- on them is lexicographic. Writes previously used time.RFC3339Nano, which
-- strips trailing zeros, so the fractional part varied in width. Wherever the
-- earlier value is a prefix of the later one the comparison inverts, because
-- 'Z' (0x5A) sorts above '0' (0x30):
--
--     "...00.5Z"  >  "...00.50000001Z"   as text
--     "...00.5Z"  <  "...00.50000001Z"   in time
--
-- The scrub queue orders on COALESCE(last_scrubbed_at, created_at), so a copy
-- could be passed over indefinitely. Cleanup claim grace periods, multipart
-- staleness cutoffs and the pending reaper's min-age compare the same columns.
--
-- Writes are canonical from this version on; this brings existing rows up to
-- the same width so old and new rows are mutually orderable. Each statement
-- skips rows already canonical, so re-running is free.

UPDATE backend_quotas SET updated_at = CASE
    WHEN instr(updated_at, '.') = 0 THEN substr(updated_at, 1, 19) || '.000000000Z'
    ELSE substr(updated_at, 1, instr(updated_at, '.'))
      || substr(substr(updated_at, instr(updated_at, '.') + 1,
                       length(updated_at) - instr(updated_at, '.') - 1) || '000000000', 1, 9)
      || 'Z'
  END
  WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE object_locations SET created_at = CASE
    WHEN instr(created_at, '.') = 0 THEN substr(created_at, 1, 19) || '.000000000Z'
    ELSE substr(created_at, 1, instr(created_at, '.'))
      || substr(substr(created_at, instr(created_at, '.') + 1,
                       length(created_at) - instr(created_at, '.') - 1) || '000000000', 1, 9)
      || 'Z'
  END
  WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE object_locations SET last_scrubbed_at = CASE
    WHEN instr(last_scrubbed_at, '.') = 0 THEN substr(last_scrubbed_at, 1, 19) || '.000000000Z'
    ELSE substr(last_scrubbed_at, 1, instr(last_scrubbed_at, '.'))
      || substr(substr(last_scrubbed_at, instr(last_scrubbed_at, '.') + 1,
                       length(last_scrubbed_at) - instr(last_scrubbed_at, '.') - 1) || '000000000', 1, 9)
      || 'Z'
  END
  WHERE last_scrubbed_at IS NOT NULL AND length(last_scrubbed_at) <> 30;

UPDATE multipart_uploads SET created_at = CASE
    WHEN instr(created_at, '.') = 0 THEN substr(created_at, 1, 19) || '.000000000Z'
    ELSE substr(created_at, 1, instr(created_at, '.'))
      || substr(substr(created_at, instr(created_at, '.') + 1,
                       length(created_at) - instr(created_at, '.') - 1) || '000000000', 1, 9)
      || 'Z'
  END
  WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE multipart_parts SET created_at = CASE
    WHEN instr(created_at, '.') = 0 THEN substr(created_at, 1, 19) || '.000000000Z'
    ELSE substr(created_at, 1, instr(created_at, '.'))
      || substr(substr(created_at, instr(created_at, '.') + 1,
                       length(created_at) - instr(created_at, '.') - 1) || '000000000', 1, 9)
      || 'Z'
  END
  WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE backend_usage SET updated_at = CASE
    WHEN instr(updated_at, '.') = 0 THEN substr(updated_at, 1, 19) || '.000000000Z'
    ELSE substr(updated_at, 1, instr(updated_at, '.'))
      || substr(substr(updated_at, instr(updated_at, '.') + 1,
                       length(updated_at) - instr(updated_at, '.') - 1) || '000000000', 1, 9)
      || 'Z'
  END
  WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE cleanup_queue SET created_at = CASE
    WHEN instr(created_at, '.') = 0 THEN substr(created_at, 1, 19) || '.000000000Z'
    ELSE substr(created_at, 1, instr(created_at, '.'))
      || substr(substr(created_at, instr(created_at, '.') + 1,
                       length(created_at) - instr(created_at, '.') - 1) || '000000000', 1, 9)
      || 'Z'
  END
  WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE cleanup_queue SET next_retry = CASE
    WHEN instr(next_retry, '.') = 0 THEN substr(next_retry, 1, 19) || '.000000000Z'
    ELSE substr(next_retry, 1, instr(next_retry, '.'))
      || substr(substr(next_retry, instr(next_retry, '.') + 1,
                       length(next_retry) - instr(next_retry, '.') - 1) || '000000000', 1, 9)
      || 'Z'
  END
  WHERE next_retry IS NOT NULL AND length(next_retry) <> 30;

UPDATE cleanup_queue SET claimed_at = CASE
    WHEN instr(claimed_at, '.') = 0 THEN substr(claimed_at, 1, 19) || '.000000000Z'
    ELSE substr(claimed_at, 1, instr(claimed_at, '.'))
      || substr(substr(claimed_at, instr(claimed_at, '.') + 1,
                       length(claimed_at) - instr(claimed_at, '.') - 1) || '000000000', 1, 9)
      || 'Z'
  END
  WHERE claimed_at IS NOT NULL AND length(claimed_at) <> 30;

UPDATE cleanup_dlq SET first_enqueued_at = CASE
    WHEN instr(first_enqueued_at, '.') = 0 THEN substr(first_enqueued_at, 1, 19) || '.000000000Z'
    ELSE substr(first_enqueued_at, 1, instr(first_enqueued_at, '.'))
      || substr(substr(first_enqueued_at, instr(first_enqueued_at, '.') + 1,
                       length(first_enqueued_at) - instr(first_enqueued_at, '.') - 1) || '000000000', 1, 9)
      || 'Z'
  END
  WHERE first_enqueued_at IS NOT NULL AND length(first_enqueued_at) <> 30;

UPDATE cleanup_dlq SET moved_at = CASE
    WHEN instr(moved_at, '.') = 0 THEN substr(moved_at, 1, 19) || '.000000000Z'
    ELSE substr(moved_at, 1, instr(moved_at, '.'))
      || substr(substr(moved_at, instr(moved_at, '.') + 1,
                       length(moved_at) - instr(moved_at, '.') - 1) || '000000000', 1, 9)
      || 'Z'
  END
  WHERE moved_at IS NOT NULL AND length(moved_at) <> 30;

UPDATE notification_outbox SET created_at = CASE
    WHEN instr(created_at, '.') = 0 THEN substr(created_at, 1, 19) || '.000000000Z'
    ELSE substr(created_at, 1, instr(created_at, '.'))
      || substr(substr(created_at, instr(created_at, '.') + 1,
                       length(created_at) - instr(created_at, '.') - 1) || '000000000', 1, 9)
      || 'Z'
  END
  WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE notification_outbox SET next_retry = CASE
    WHEN instr(next_retry, '.') = 0 THEN substr(next_retry, 1, 19) || '.000000000Z'
    ELSE substr(next_retry, 1, instr(next_retry, '.'))
      || substr(substr(next_retry, instr(next_retry, '.') + 1,
                       length(next_retry) - instr(next_retry, '.') - 1) || '000000000', 1, 9)
      || 'Z'
  END
  WHERE next_retry IS NOT NULL AND length(next_retry) <> 30;

UPDATE pending_objects SET created_at = CASE
    WHEN instr(created_at, '.') = 0 THEN substr(created_at, 1, 19) || '.000000000Z'
    ELSE substr(created_at, 1, instr(created_at, '.'))
      || substr(substr(created_at, instr(created_at, '.') + 1,
                       length(created_at) - instr(created_at, '.') - 1) || '000000000', 1, 9)
      || 'Z'
  END
  WHERE created_at IS NOT NULL AND length(created_at) <> 30;
