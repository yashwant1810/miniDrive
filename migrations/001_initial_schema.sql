-- +goose Up

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Users
CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email           VARCHAR(255) NOT NULL UNIQUE,
    password_hash   VARCHAR(255) NOT NULL,
    quota_bytes     BIGINT NOT NULL DEFAULT 16106127360,
    used_bytes      BIGINT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Nodes (files + folders)
CREATE TABLE nodes (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id            UUID NOT NULL REFERENCES users(id),
    parent_id           UUID REFERENCES nodes(id),
    name                VARCHAR(255) NOT NULL,
    type                VARCHAR(10) NOT NULL CHECK (type IN ('file', 'folder')),
    current_version_id  UUID,
    deleted_at          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_nodes_parent_name ON nodes (parent_id, name) WHERE deleted_at IS NULL;
CREATE INDEX idx_nodes_parent_id ON nodes (parent_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_nodes_owner_id ON nodes (owner_id);
CREATE INDEX idx_nodes_trash ON nodes (owner_id, deleted_at) WHERE deleted_at IS NOT NULL;

-- Versions
CREATE TABLE versions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id     UUID NOT NULL REFERENCES nodes(id),
    size        BIGINT NOT NULL,
    status      VARCHAR(10) NOT NULL DEFAULT 'uploading' CHECK (status IN ('uploading', 'committed')),
    created_by  UUID NOT NULL REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_versions_node_id ON versions (node_id, created_at DESC);
CREATE INDEX idx_versions_uploading ON versions (created_at) WHERE status = 'uploading';

-- Chunks
CREATE TABLE chunks (
    hash        CHAR(64) PRIMARY KEY,
    size        INT NOT NULL,
    s3_key      VARCHAR(255) NOT NULL,
    ref_count   INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_chunks_orphan ON chunks (ref_count) WHERE ref_count = 0;

-- Version-Chunk mapping
CREATE TABLE version_chunks (
    version_id  UUID NOT NULL REFERENCES versions(id) ON DELETE CASCADE,
    chunk_index INT NOT NULL,
    chunk_hash  CHAR(64) NOT NULL REFERENCES chunks(hash),
    PRIMARY KEY (version_id, chunk_index)
);

CREATE INDEX idx_vc_chunk_hash ON version_chunks (chunk_hash);

-- Shares
CREATE TABLE shares (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id     UUID NOT NULL REFERENCES nodes(id),
    grantee_id  UUID NOT NULL REFERENCES users(id),
    role        VARCHAR(10) NOT NULL CHECK (role IN ('viewer', 'editor')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (node_id, grantee_id)
);

CREATE INDEX idx_shares_node_id ON shares (node_id);
CREATE INDEX idx_shares_grantee_id ON shares (grantee_id);

-- Change log (sync)
CREATE TABLE change_log (
    seq         BIGSERIAL PRIMARY KEY,
    user_id     UUID NOT NULL REFERENCES users(id),
    node_id     UUID NOT NULL REFERENCES nodes(id),
    op          VARCHAR(20) NOT NULL CHECK (op IN ('create', 'update', 'move', 'delete', 'restore', 'share', 'unshare')),
    metadata    JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_changelog_user_seq ON change_log (user_id, seq);
CREATE INDEX idx_changelog_created_at ON change_log (created_at);

-- Refresh tokens
CREATE TABLE refresh_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id),
    token_hash  CHAR(64) NOT NULL UNIQUE,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_rt_user_id ON refresh_tokens (user_id);
CREATE INDEX idx_rt_expires_at ON refresh_tokens (expires_at);

-- +goose Down

DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS change_log;
DROP TABLE IF EXISTS shares;
DROP TABLE IF EXISTS version_chunks;
DROP TABLE IF EXISTS chunks;
DROP TABLE IF EXISTS versions;
DROP TABLE IF EXISTS nodes;
DROP TABLE IF EXISTS users;
