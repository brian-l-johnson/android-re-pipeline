-- +goose Up
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS mobsf_scan_hash TEXT;

-- +goose Down
ALTER TABLE jobs DROP COLUMN IF EXISTS mobsf_scan_hash;
