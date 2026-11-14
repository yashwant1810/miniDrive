# MiniDrive — Database Schema (v1)

See [ER Diagram](Task4DatabaseSchema.drawio) for the visual layout.

## Tables

### 1. users

The account table. One row per registered user.

```sql
CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email           VARCHAR(255) NOT NULL UNIQUE,
    password_hash   VARCHAR(255) NOT NULL,
    quota_bytes     BIGINT NOT NULL DEFAULT 16106127360,  -- 15 GB
    used_bytes      BIGINT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Notes:
- `password_hash`: bcrypt output (~60 chars), VARCHAR(255) gives room
- `quota_bytes`: default 15 GB (matches Google Drive free tier)
- `used_bytes`: tracked at application level, incremented on commit, decremented on purge

### 2. nodes

Files and folders in a single table. Self-referential for the folder tree.

```sql
CREATE TABLE nodes (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id            UUID NOT NULL REFERENCES users(id),
    parent_id           UUID REFERENCES nodes(id),          -- NULL = root level
    name                VARCHAR(255) NOT NULL,
    type                VARCHAR(10) NOT NULL CHECK (type IN ('file', 'folder')),
    current_version_id  UUID,                                -- NULL for folders, set on commit for files
    deleted_at          TIMESTAMPTZ,                         -- NULL = not deleted
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- No duplicate names in the same folder (only among non-deleted nodes)
CREATE UNIQUE INDEX idx_nodes_parent_name
    ON nodes (parent_id, name)
    WHERE deleted_at IS NULL;

-- List contents of a folder
CREATE INDEX idx_nodes_parent_id ON nodes (parent_id) WHERE deleted_at IS NULL;

-- All nodes owned by a user (for quota, listing root)
CREATE INDEX idx_nodes_owner_id ON nodes (owner_id);

-- Trash view: find deleted nodes for a user
CREATE INDEX idx_nodes_trash ON nodes (owner_id, deleted_at) WHERE deleted_at IS NOT NULL;
```

Design decisions:
- **Single table for files and folders** — simpler than two tables. The `type` column distinguishes them. Folders ignore `current_version_id`. Files always have one.
- **Self-referential parent_id** — `NULL` means root level for that user. The folder tree is a standard adjacency list.
- **Partial unique index** — `WHERE deleted_at IS NULL` allows a deleted file and a new file with the same name to coexist. Only non-deleted names must be unique per folder.
- **current_version_id is nullable** — not a foreign key constraint to avoid a circular dependency (nodes ↔ versions). Enforced at the application level.
- **updated_at** — set by the application on every metadata change (rename, move, new version). Used for display, not for sync (sync uses the change_log).

### 3. versions

One row per file version. A file can have up to 10 versions (older ones are pruned).

```sql
CREATE TABLE versions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id     UUID NOT NULL REFERENCES nodes(id),
    size        BIGINT NOT NULL,
    status      VARCHAR(10) NOT NULL DEFAULT 'uploading'
                    CHECK (status IN ('uploading', 'committed')),
    created_by  UUID NOT NULL REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- All versions for a file, newest first
CREATE INDEX idx_versions_node_id ON versions (node_id, created_at DESC);

-- Find stale uploads for cleanup
CREATE INDEX idx_versions_uploading ON versions (created_at)
    WHERE status = 'uploading';
```

Design decisions:
- **status column** — tracks the upload lifecycle. Only `committed` versions are visible in sync and downloads. `uploading` versions are candidates for orphan cleanup if they're older than 24 hours.
- **No version number column** — versions are ordered by `created_at`. Restoring a version creates a new row (with a new `created_at`), which naturally becomes the latest.
- **created_by** — tracks who created this version. Important for shared files where multiple users can upload.

### 4. chunks

Content-addressed blob references. One row per unique chunk across the entire system.

```sql
CREATE TABLE chunks (
    hash        CHAR(64) PRIMARY KEY,           -- SHA-256 hex digest
    size        INT NOT NULL,
    s3_key      VARCHAR(255) NOT NULL,           -- e.g., "ab/cd/abcdef1234..."
    ref_count   INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Find chunks eligible for deletion
CREATE INDEX idx_chunks_orphan ON chunks (ref_count) WHERE ref_count = 0;
```

Design decisions:
- **hash as primary key** — content-addressed. Two identical chunks (same bytes) produce the same hash and share one row. This is the dedup mechanism.
- **s3_key** — the actual S3 object key. Uses prefix distribution: first 2 chars + next 2 chars as directories (`ab/cd/abcdef...`) to distribute load across S3 partitions.
- **ref_count** — how many `version_chunks` rows reference this chunk. Incremented on commit, decremented on version purge. When it hits 0, the chunk is eligible for S3 deletion.
- **size is INT not BIGINT** — max chunk size is 4 MB (4,194,304 bytes), well within INT range.

### 5. version_chunks

Join table linking versions to their ordered chunks.

```sql
CREATE TABLE version_chunks (
    version_id  UUID NOT NULL REFERENCES versions(id) ON DELETE CASCADE,
    chunk_index INT NOT NULL,
    chunk_hash  CHAR(64) NOT NULL REFERENCES chunks(hash),
    PRIMARY KEY (version_id, chunk_index)
);

-- Look up all versions referencing a specific chunk (for ref_count verification)
CREATE INDEX idx_vc_chunk_hash ON version_chunks (chunk_hash);
```

Design decisions:
- **Composite primary key** — (version_id, chunk_index) is unique and is the natural key. No surrogate ID needed.
- **chunk_index** — ordering matters. The client reassembles the file by downloading chunks in index order.
- **ON DELETE CASCADE** — when a version is hard-deleted (trash purge), its chunk mappings are automatically removed. The application must then decrement `ref_count` on each referenced chunk.
- **Index on chunk_hash** — needed for the cleanup job to verify ref_count before deleting a chunk from S3.

### 6. shares

ACL entries. One row per (node, grantee) pair.

```sql
CREATE TABLE shares (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id     UUID NOT NULL REFERENCES nodes(id),
    grantee_id  UUID NOT NULL REFERENCES users(id),
    role        VARCHAR(10) NOT NULL CHECK (role IN ('viewer', 'editor')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (node_id, grantee_id)
);

-- All shares for a node (list shares dialog)
CREATE INDEX idx_shares_node_id ON shares (node_id);

-- All shares granted to a user (shared-with-me view)
CREATE INDEX idx_shares_grantee_id ON shares (grantee_id);
```

Design decisions:
- **UNIQUE (node_id, grantee_id)** — a user can only have one share per node. To change the role, update the existing row.
- **No cascading inheritance rows** — sharing a folder doesn't create rows for every child. Inheritance is resolved at query time by walking the parent chain. This keeps writes fast and avoids consistency issues when moving files.
- **role as VARCHAR with CHECK** — extensible. Adding a new role (e.g., `commenter`) is an ALTER CHECK, not a schema migration.

### 7. change_log

Append-only log for the sync protocol. Clients poll this via `GET /changes?cursor=N`.

```sql
CREATE TABLE change_log (
    seq         BIGSERIAL PRIMARY KEY,
    user_id     UUID NOT NULL REFERENCES users(id),
    node_id     UUID NOT NULL REFERENCES nodes(id),
    op          VARCHAR(20) NOT NULL
                    CHECK (op IN ('create', 'update', 'move', 'delete', 'restore', 'share', 'unshare')),
    metadata    JSONB,                          -- op-specific data (new name, new parent, etc.)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The sync query: changes for a user after a cursor, ordered by seq
CREATE INDEX idx_changelog_user_seq ON change_log (user_id, seq);

-- Prune old entries
CREATE INDEX idx_changelog_created_at ON change_log (created_at);
```

Design decisions:
- **BIGSERIAL** — auto-incrementing 64-bit integer. Monotonically increasing. This IS the cursor. No gaps (within a transaction), globally ordered.
- **user_id** — the user who should see this change. For shared files, multiple change_log entries are written (one per user with access). This denormalization makes the sync query fast: just `WHERE user_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`.
- **metadata JSONB** — flexible. A `move` op stores `{"old_parent_id": "...", "new_parent_id": "..."}`. A `share` op stores `{"grantee_id": "...", "role": "..."}`. Avoids adding columns for every op type.
- **Append-only** — rows are never updated or deleted during normal operation. Only the pruning job deletes old entries.

### 8. refresh_tokens

Server-side store for refresh tokens. Enables revocation.

```sql
CREATE TABLE refresh_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id),
    token_hash  CHAR(64) NOT NULL UNIQUE,       -- SHA-256 of the actual token
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Find tokens by user (for revoke-all on password change)
CREATE INDEX idx_rt_user_id ON refresh_tokens (user_id);

-- Clean up expired tokens
CREATE INDEX idx_rt_expires_at ON refresh_tokens (expires_at);
```

Design decisions:
- **token_hash, not the token itself** — the raw refresh token is only ever seen by the client. The server stores a SHA-256 hash. If the DB is compromised, the tokens are useless.
- **expires_at** — 7 days from issuance. A background job deletes expired rows.
- **Revocation** — delete the row. On password change, delete all rows for that user_id.

---

## Relationships Summary

```
users ──1:N──► nodes          (owner_id)
users ──1:N──► refresh_tokens (user_id)
users ──1:N──► change_log     (user_id)
users ──1:N──► shares         (grantee_id)
users ──1:N──► versions       (created_by)

nodes ──1:N──► nodes          (parent_id, self-referential)
nodes ──1:N──► versions       (node_id)
nodes ──1:N──► shares         (node_id)
nodes ──1:N──► change_log     (node_id)
nodes ──0:1──► versions       (current_version_id)

versions ──1:N──► version_chunks (version_id)
chunks   ──1:N──► version_chunks (chunk_hash)
```

## Index Summary

| Table | Index | Purpose |
|---|---|---|
| nodes | `(parent_id, name) WHERE deleted_at IS NULL` | Unique names per folder |
| nodes | `(parent_id) WHERE deleted_at IS NULL` | List folder contents |
| nodes | `(owner_id)` | User's files, root listing |
| nodes | `(owner_id, deleted_at) WHERE deleted_at IS NOT NULL` | Trash view |
| versions | `(node_id, created_at DESC)` | Version history for a file |
| versions | `(created_at) WHERE status = 'uploading'` | Orphan cleanup |
| version_chunks | `(chunk_hash)` | Ref count verification |
| chunks | `(ref_count) WHERE ref_count = 0` | Chunk garbage collection |
| shares | `(node_id)` | List shares for a node |
| shares | `(grantee_id)` | Shared-with-me view |
| change_log | `(user_id, seq)` | Sync polling query |
| change_log | `(created_at)` | Log pruning |
| refresh_tokens | `(user_id)` | Revoke all tokens |
| refresh_tokens | `(expires_at)` | Expired token cleanup |

## Migration File

The full SQL above is the v1 migration. In the project, it will live at:

```
migrations/001_initial_schema.sql
```

Run via `goose up` against Postgres (or MinIO-backed local Postgres in Docker).
