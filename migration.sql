-- -------------------------------------------------------------------------------
-- S3 Proxy Database Schema
--
-- Project: Munchbox / Author: Alex Freidah
--
-- PostgreSQL schema for quota tracking and object location storage.
-- Run this migration before starting the s3-proxy service.
-- -------------------------------------------------------------------------------

-- Track quota usage per backend
CREATE TABLE IF NOT EXISTS backend_quotas (
    backend_name TEXT PRIMARY KEY,
    bytes_used   BIGINT NOT NULL DEFAULT 0,
    bytes_limit  BIGINT NOT NULL,
    updated_at   TIMESTAMPTZ DEFAULT NOW()
);

-- Track which backend stores which object
CREATE TABLE IF NOT EXISTS object_locations (
    object_key   TEXT PRIMARY KEY,
    backend_name TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    size_bytes   BIGINT NOT NULL,
    created_at   TIMESTAMPTZ DEFAULT NOW()
);

-- Index for efficient backend lookups
CREATE INDEX IF NOT EXISTS idx_object_locations_backend
    ON object_locations(backend_name);

-- Index for cleanup queries
CREATE INDEX IF NOT EXISTS idx_object_locations_created
    ON object_locations(created_at);
