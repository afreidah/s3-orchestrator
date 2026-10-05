-- Reconcile records an envelope that no key opens as encrypted with no key.
-- Import now marks such a row unmanaged, so it keeps its quota without being
-- replicated, listed or served. This brings rows imported before that change
-- into line.

UPDATE object_locations
   SET managed = 0
 WHERE encrypted
   AND (encryption_key IS NULL OR length(encryption_key) = 0)
   AND managed;
