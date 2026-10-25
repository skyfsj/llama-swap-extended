-- +goose Up
ALTER TABLE response_affinity ADD COLUMN request_json BLOB NOT NULL DEFAULT X'';

-- +goose Down
ALTER TABLE response_affinity DROP COLUMN request_json;
