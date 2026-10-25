-- +goose Up
CREATE TABLE protocol_file_blobs (digest TEXT PRIMARY KEY, content BLOB NOT NULL);
CREATE TABLE protocol_files (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    digest TEXT NOT NULL REFERENCES protocol_file_blobs(digest),
    filename TEXT NOT NULL,
    mime_type TEXT NOT NULL,
    bytes INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX idx_protocol_files_owner ON protocol_files(owner_id,created_at);
CREATE TABLE protocol_file_references (
    file_id TEXT NOT NULL REFERENCES protocol_files(id) ON DELETE CASCADE,
    scope TEXT NOT NULL,
    reference TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    PRIMARY KEY(file_id,scope)
);
-- +goose Down
DROP TABLE protocol_file_references;
DROP TABLE protocol_files;
DROP TABLE protocol_file_blobs;
