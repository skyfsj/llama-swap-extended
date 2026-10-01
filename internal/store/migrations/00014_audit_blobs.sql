-- +goose Up
-- Audit request/response bodies can embed multi-megabyte base64 media.
-- Content-addressed blob files (see BlobStore) keep those out of the SQLite
-- BLOB columns; the two TEXT columns hold the sha256 reference, empty when
-- the body is stored inline (legacy rows, in-memory stores, or a failed
-- blob write).
ALTER TABLE audit_conversations ADD COLUMN req_blob TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_conversations ADD COLUMN resp_blob TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite cannot drop a column on every supported version. The references are
-- intentionally retained on downgrade; rows written by newer binaries keep
-- their blob files, which remain valid for re-upgrade.
