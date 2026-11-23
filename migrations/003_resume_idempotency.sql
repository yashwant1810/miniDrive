-- +goose Up

-- Add idempotency key to versions for resumable uploads.
-- A client re-sending the same Idempotency-Key can pick up where it left off.
ALTER TABLE versions ADD COLUMN idempotency_key VARCHAR(255);

-- Only one uploading version per user per idempotency key.
-- Committed versions don't block future keys.
CREATE UNIQUE INDEX idx_versions_idempotency
    ON versions (created_by, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND status = 'uploading';

-- +goose Down

DROP INDEX IF EXISTS idx_versions_idempotency;
ALTER TABLE versions DROP COLUMN IF EXISTS idempotency_key;
