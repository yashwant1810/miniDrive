# MiniDrive — Local Dev Setup (v1)

## Prerequisites

| Tool | Version | Install |
|---|---|---|
| Go | 1.23+ | `brew install go` |
| Docker + Docker Compose | Latest | [Docker Desktop](https://www.docker.com/products/docker-desktop/) |
| goose | Latest | `go install github.com/pressly/goose/v3/cmd/goose@latest` |
| psql | Any | Comes with `brew install postgresql` (client only, no server needed) |

---

## 1. Docker Compose

Postgres and MinIO run in containers. The API server runs natively (faster iteration, easier debugging).

### docker-compose.yml

```yaml
version: "3.9"

services:
  postgres:
    image: postgres:16-alpine
    container_name: minidrive-postgres
    environment:
      POSTGRES_USER: minidrive
      POSTGRES_PASSWORD: minidrive
      POSTGRES_DB: minidrive
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U minidrive"]
      interval: 5s
      timeout: 3s
      retries: 5

  minio:
    image: minio/minio:latest
    container_name: minidrive-minio
    command: server /data --console-address ":9001"
    environment:
      MINIO_ROOT_USER: minioadmin
      MINIO_ROOT_PASSWORD: minioadmin
    ports:
      - "9000:9000"    # S3 API
      - "9001:9001"    # MinIO web console
    volumes:
      - minio-data:/data
    healthcheck:
      test: ["CMD", "mc", "ready", "local"]
      interval: 5s
      timeout: 3s
      retries: 5

  # One-time setup: create the S3 bucket
  minio-init:
    image: minio/mc:latest
    depends_on:
      minio:
        condition: service_healthy
    entrypoint: >
      /bin/sh -c "
      mc alias set local http://minio:9000 minioadmin minioadmin;
      mc mb local/minidrive-chunks --ignore-existing;
      echo 'Bucket minidrive-chunks ready';
      "

volumes:
  pgdata:
  minio-data:
```

---

## 2. Environment Config

### .env.example

```bash
# Server
PORT=8080

# Database
DATABASE_URL=postgres://minidrive:minidrive@localhost:5432/minidrive?sslmode=disable

# S3 / MinIO
S3_ENDPOINT=http://localhost:9000
S3_BUCKET=minidrive-chunks
S3_REGION=us-east-1
S3_ACCESS_KEY=minioadmin
S3_SECRET_KEY=minioadmin
S3_USE_PATH_STYLE=true          # Required for MinIO

# Auth
JWT_SECRET=dev-secret-change-in-production-minimum-32-chars!!

# Cleanup
ORPHAN_CLEANUP_INTERVAL=6h
ORPHAN_MAX_AGE=24h
```

Copy to `.env` before running:

```bash
cp .env.example .env
```

`S3_USE_PATH_STYLE=true` is required for MinIO. AWS S3 uses virtual-hosted style by default. The `s3client` package reads this flag and configures the SDK accordingly.

---

## 3. Migrations

### migrations/001_initial_schema.sql

Goose format with `-- +goose Up` and `-- +goose Down` markers:

```sql
-- +goose Up

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email           VARCHAR(255) NOT NULL UNIQUE,
    password_hash   VARCHAR(255) NOT NULL,
    quota_bytes     BIGINT NOT NULL DEFAULT 16106127360,
    used_bytes      BIGINT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

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

CREATE TABLE chunks (
    hash        CHAR(64) PRIMARY KEY,
    size        INT NOT NULL,
    s3_key      VARCHAR(255) NOT NULL,
    ref_count   INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_chunks_orphan ON chunks (ref_count) WHERE ref_count = 0;

CREATE TABLE version_chunks (
    version_id  UUID NOT NULL REFERENCES versions(id) ON DELETE CASCADE,
    chunk_index INT NOT NULL,
    chunk_hash  CHAR(64) NOT NULL REFERENCES chunks(hash),
    PRIMARY KEY (version_id, chunk_index)
);

CREATE INDEX idx_vc_chunk_hash ON version_chunks (chunk_hash);

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
```

---

## 4. Seed Data

### scripts/seed.sql

```sql
-- Test user: alice@test.com / password123
INSERT INTO users (id, email, password_hash)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    'alice@test.com',
    -- bcrypt hash of "password123" (cost 12)
    '$2a$12$LJ3m4ys3Lk0TSwMC/VAPquGpGjRHVJ.sI0VBs7BYxNMYxJ4GQqMi2'
);

-- Test user: bob@test.com / password123
INSERT INTO users (id, email, password_hash)
VALUES (
    '00000000-0000-0000-0000-000000000002',
    'bob@test.com',
    '$2a$12$LJ3m4ys3Lk0TSwMC/VAPquGpGjRHVJ.sI0VBs7BYxNMYxJ4GQqMi2'
);

-- Alice's root folders
INSERT INTO nodes (id, owner_id, parent_id, name, type)
VALUES
    ('00000000-0000-0000-0000-000000000010', '00000000-0000-0000-0000-000000000001', NULL, 'Documents', 'folder'),
    ('00000000-0000-0000-0000-000000000011', '00000000-0000-0000-0000-000000000001', NULL, 'Photos', 'folder');

-- Subfolder
INSERT INTO nodes (id, owner_id, parent_id, name, type)
VALUES
    ('00000000-0000-0000-0000-000000000012', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000010', 'Reports', 'folder');

-- Share Documents folder with Bob as editor
INSERT INTO shares (node_id, grantee_id, role)
VALUES ('00000000-0000-0000-0000-000000000010', '00000000-0000-0000-0000-000000000002', 'editor');
```

---

## 5. Scripts

### scripts/reset.sh

```bash
#!/bin/bash
set -e

source .env

echo "Dropping and recreating database..."
psql "$DATABASE_URL" -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"

echo "Running migrations..."
goose -dir migrations postgres "$DATABASE_URL" up

echo "Seeding test data..."
psql "$DATABASE_URL" -f scripts/seed.sql

echo "Done. Database reset."
```

---

## 6. Dev Workflow

### First-time setup

```bash
# 1. Clone the repo
git clone git@github.com:yashwant1810/miniDrive.git
cd miniDrive

# 2. Copy env config
cp .env.example .env

# 3. Start Postgres + MinIO
docker compose up -d

# 4. Wait for healthy (check with)
docker compose ps

# 5. Run migrations
goose -dir migrations postgres "postgres://minidrive:minidrive@localhost:5432/minidrive?sslmode=disable" up

# 6. Seed test data
psql "postgres://minidrive:minidrive@localhost:5432/minidrive?sslmode=disable" -f scripts/seed.sql

# 7. Run the API server
go run ./cmd/server
```

Server is now running at `http://localhost:8080`.

### Daily workflow

```bash
# Start containers (if not running)
docker compose up -d

# Run server
go run ./cmd/server

# In another terminal — test with curl
curl -X POST http://localhost:8080/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"alice@test.com","password":"password123"}'
```

### Reset everything

```bash
# Nuke DB and reseed
./scripts/reset.sh

# Nuke MinIO data (delete all chunks)
docker compose down -v
docker compose up -d
```

### Run tests

```bash
go test ./...

# With verbose output
go test -v ./...

# Single package
go test -v ./internal/storage/...
```

---

## 7. MinIO Web Console

MinIO has a built-in web UI for browsing buckets and objects.

- URL: http://localhost:9001
- Username: `minioadmin`
- Password: `minioadmin`

Use it to:
- Verify chunks were uploaded after a test upload
- Inspect chunk keys (e.g., `ab/cd/abcdef1234...`)
- Manually delete chunks to test error handling
- Check bucket size

---

## 8. Useful Commands

| Task | Command |
|---|---|
| Start containers | `docker compose up -d` |
| Stop containers | `docker compose down` |
| Stop + delete data | `docker compose down -v` |
| View container logs | `docker compose logs -f postgres` |
| Run migrations up | `make migrate` |
| Reset DB | `make reset` |
| Build both binaries | `make build` |
| Run API server | `make run` |
| Run tests | `make test` |
| Connect to DB | `psql "postgres://minidrive:minidrive@localhost:5432/minidrive?sslmode=disable"` |
| Check MinIO buckets | Open http://localhost:9001 |

---

## 9. Testing Strategy

### Unit tests

Each `internal/` package has `*_test.go` files. Use the `Store` and `S3Client` interfaces for mocking.

```go
// Mock store for handler tests
type mockStore struct {
    storage.Store  // embed interface, override what you need
}

func (m *mockStore) GetNode(ctx context.Context, id uuid.UUID) (*model.Node, error) {
    return &model.Node{ID: id, Name: "test.pdf", Type: "file"}, nil
}
```

### Integration tests

Test against real Postgres + MinIO using Docker. Use a separate test database:

```bash
# Create test DB
psql "postgres://minidrive:minidrive@localhost:5432/minidrive?sslmode=disable" \
  -c "CREATE DATABASE minidrive_test;"

# Run migrations on test DB
goose -dir migrations postgres "postgres://minidrive:minidrive@localhost:5432/minidrive_test?sslmode=disable" up
```

Set `DATABASE_URL` to the test DB in test config. Each test cleans up after itself with `TRUNCATE` statements.

### End-to-end tests

Use the CLI against the running API server:

```bash
# Login
./bin/minidrive login --email alice@test.com --password password123

# Upload a file
./bin/minidrive upload testfile.txt

# Download it back
./bin/minidrive download <file_id> --output downloaded.txt

# Compare
diff testfile.txt downloaded.txt
```
