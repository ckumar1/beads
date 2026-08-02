-- Clone-local wisps can be created by ignored migrations after main migration
-- 0054 has already run. Mirror the route hash/index here so every ready-work
-- table satisfies the shared metadata predicate.

SET @has_wisps = (
    SELECT IF(COUNT(*) > 0, 1, 0)
    FROM INFORMATION_SCHEMA.TABLES
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'wisps'
);
SET @wisps_needs_col = (
    SELECT IF(COUNT(*) = 0, 1, 0)
    FROM INFORMATION_SCHEMA.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'wisps'
      AND COLUMN_NAME = 'gc_routed_to_hash'
);
SET @sql = IF(
    @has_wisps = 1 AND @wisps_needs_col = 1,
    'ALTER TABLE wisps ADD COLUMN gc_routed_to_hash BINARY(32) AS (UNHEX(SHA2(JSON_UNQUOTE(JSON_EXTRACT(metadata, ''$."gc.routed_to"'')), 256))) STORED',
    'SELECT 1'
);
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @wisps_needs_idx = (
    SELECT IF(COUNT(*) = 0, 1, 0)
    FROM INFORMATION_SCHEMA.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'wisps'
      AND INDEX_NAME = 'idx_wisps_gc_routed_to_hash'
);
SET @sql = IF(
    @has_wisps = 1 AND @wisps_needs_idx = 1,
    'CREATE INDEX idx_wisps_gc_routed_to_hash ON wisps (gc_routed_to_hash, status)',
    'SELECT 1'
);
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
