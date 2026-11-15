-- +goose Up

-- The original unique index on (parent_id, name) doesn't catch duplicates
-- at root level because NULL != NULL in Postgres. This index covers that case.
CREATE UNIQUE INDEX idx_nodes_root_name
    ON nodes (owner_id, name)
    WHERE parent_id IS NULL AND deleted_at IS NULL;

-- +goose Down

DROP INDEX IF EXISTS idx_nodes_root_name;
