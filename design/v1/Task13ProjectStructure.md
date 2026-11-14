# MiniDrive — Project Structure (v1)

## Go Module

```
module github.com/yashwant1810/miniDrive
go 1.23
```

Single Go module. Both the API server and CLI client live in the same module and share internal packages.

---

## Directory Layout

```
miniDrive/
├── cmd/
│   ├── server/
│   │   └── main.go              # API server entry point
│   └── cli/
│       └── main.go              # CLI client entry point (cobra root command)
│
├── internal/
│   ├── config/
│   │   └── config.go            # Config struct, Load() from env vars
│   │
│   ├── model/
│   │   ├── user.go              # User struct
│   │   ├── node.go              # Node struct (file + folder)
│   │   ├── version.go           # Version struct
│   │   ├── chunk.go             # Chunk, VersionChunk structs
│   │   ├── share.go             # Share struct
│   │   ├── change.go            # ChangeLogEntry struct
│   │   └── auth.go              # Claims, RefreshToken structs
│   │
│   ├── storage/
│   │   ├── store.go             # Store interface (all DB methods)
│   │   ├── pgstore.go           # PgStore implements Store using pgx
│   │   ├── user.go              # CreateUser, GetUserByEmail, UpdatePassword
│   │   ├── node.go              # CreateNode, GetNode, ListFolder, MoveNode, SoftDelete
│   │   ├── version.go           # CreateVersion, CommitVersion, ListVersions, RestoreVersion
│   │   ├── chunk.go             # UpsertChunks, GetChunksByHash, DecrementRefCount
│   │   ├── share.go             # CreateShare, DeleteShare, ListShares, ResolveRole
│   │   ├── changelog.go         # InsertChange, GetChanges
│   │   ├── trash.go             # ListTrash, RestoreFromTrash, PurgeFromTrash
│   │   └── token.go             # CreateRefreshToken, ValidateRefreshToken, RevokeTokens
│   │
│   ├── s3client/
│   │   ├── client.go            # S3Client interface + Client struct
│   │   ├── presign.go           # GeneratePresignedUploadURL, GeneratePresignedDownloadURL
│   │   └── chunks.go            # CheckChunkExists, DeleteChunk, HeadChunk
│   │
│   ├── auth/
│   │   ├── jwt.go               # GenerateAccessToken, ParseToken, Claims
│   │   ├── password.go          # HashPassword, CheckPassword
│   │   └── middleware.go        # AuthMiddleware (http.Handler wrapper)
│   │
│   ├── acl/
│   │   └── acl.go               # CheckPermission, ResolveRole (recursive CTE)
│   │
│   ├── handler/
│   │   ├── handler.go           # Handler struct (holds Store, S3Client, Config)
│   │   ├── routes.go            # RegisterRoutes — maps paths to handlers
│   │   ├── auth.go              # handleSignup, handleLogin, handleRefresh, handleLogout
│   │   ├── file.go              # handleInitUpload, handleCommit, handleDownload
│   │   ├── folder.go            # handleCreateFolder, handleListFolder, handleUpdateFolder, handleDeleteFolder
│   │   ├── share.go             # handleGrantShare, handleRevokeShare, handleUpdateShare, handleListShares
│   │   ├── version.go           # handleListVersions, handleRestoreVersion
│   │   ├── sync.go              # handleGetChanges
│   │   ├── trash.go             # handleListTrash, handleRestore, handlePurge
│   │   └── middleware.go        # RateLimiter, RequestID, Logger (non-auth middleware)
│   │
│   └── cleanup/
│       └── cleanup.go           # OrphanCleanupJob — deletes stale uploads + unreferenced chunks
│
├── migrations/
│   └── 001_initial_schema.sql   # Full CREATE TABLE statements (goose format)
│
├── web/                          # Next.js app (Phase 6+, empty for now)
│   └── .gitkeep
│
├── scripts/
│   ├── seed.sql                 # Test user + sample folder/file data
│   └── reset.sh                 # Drop and recreate DB, re-run migrations + seed
│
├── docker-compose.yml            # Postgres + MinIO for local dev
├── .env.example                  # All env vars with placeholder values
├── .gitignore
├── go.mod
├── go.sum
├── Makefile                      # Build, run, test, migrate commands
├── README.md
│
├── docs/
│   └── tech-stack.md
│
├── design/
│   └── v1/
│       └── (all design docs)
│
└── RealWorldQuestions.md
```

---

## Package Responsibilities

### cmd/server

API server entry point. Wires everything together and starts the HTTP server.

```go
func main() {
    cfg := config.Load()
    pool := connectDB(cfg.DatabaseURL)
    s3 := s3client.New(cfg.S3Endpoint, cfg.S3Bucket, cfg.S3Region)
    store := storage.NewPgStore(pool)

    h := handler.New(store, s3, cfg)
    mux := http.NewServeMux()
    h.RegisterRoutes(mux)

    // Start cleanup job in background
    go cleanup.RunOrphanCleanup(store, s3, 6*time.Hour)

    log.Printf("listening on :%s", cfg.Port)
    http.ListenAndServe(":"+cfg.Port, mux)
}
```

No business logic here — just wiring.

### cmd/cli

CLI client using cobra. Subcommands:

```
minidrive login
minidrive signup
minidrive upload <file> [--folder <id>]
minidrive download <file_id> [--version <id>] [--output <path>]
minidrive ls [folder_id]
minidrive mkdir <name> [--parent <id>]
minidrive mv <id> <new_parent_id>
minidrive rm <id>
minidrive share <id> <email> --role <viewer|editor>
minidrive unshare <share_id>
minidrive sync <local_folder>
minidrive versions <file_id>
minidrive trash
minidrive restore <id>
```

Each subcommand calls the API and/or S3 directly (for upload/download).

### internal/model

Pure data types. No logic, no dependencies. Shared by every other package.

```go
type Node struct {
    ID               uuid.UUID
    OwnerID          uuid.UUID
    ParentID         *uuid.UUID  // nil = root
    Name             string
    Type             string      // "file" or "folder"
    CurrentVersionID *uuid.UUID  // nil for folders
    DeletedAt        *time.Time  // nil = active
    CreatedAt        time.Time
    UpdatedAt        time.Time
}
```

### internal/storage

All Postgres queries. The `Store` interface allows mocking for tests.

```go
type Store interface {
    // Users
    CreateUser(ctx context.Context, email, passwordHash string) (*model.User, error)
    GetUserByEmail(ctx context.Context, email string) (*model.User, error)

    // Nodes
    CreateNode(ctx context.Context, node *model.Node) error
    GetNode(ctx context.Context, id uuid.UUID) (*model.Node, error)
    ListFolder(ctx context.Context, parentID uuid.UUID, cursor string, limit int) ([]model.Node, string, error)
    // ... etc
}
```

`PgStore` implements `Store` using pgx. Queries are written as raw SQL (no ORM), generated by sqlc or hand-written.

### internal/s3client

S3 operations. Interface for mocking in tests.

```go
type S3Client interface {
    GeneratePresignedUploadURL(ctx context.Context, key string, expiry time.Duration) (string, error)
    GeneratePresignedDownloadURL(ctx context.Context, key string, expiry time.Duration) (string, error)
    CheckChunkExists(ctx context.Context, key string) (bool, error)
    HeadChunk(ctx context.Context, key string) (size int64, err error)
    DeleteChunk(ctx context.Context, key string) error
}
```

Works with both S3 and MinIO — same API, different endpoint URL.

### internal/handler

HTTP handlers. Each handler:
1. Parses the request
2. Calls `acl.CheckPermission` if needed
3. Calls `store` and/or `s3client` methods
4. Writes the JSON response

Handlers never import `pgx` or `aws-sdk` directly — they go through the interfaces.

### internal/acl

Permission resolution. Depends on `storage` for the recursive CTE query.

### internal/auth

JWT generation/validation and password hashing. The middleware lives here because it's auth-specific.

### internal/cleanup

Background job. Runs on a ticker in the server process. Deletes `uploading` versions older than 24h and chunks with `ref_count = 0`.

---

## Build Targets

### Makefile

```makefile
.PHONY: build run test migrate seed reset clean

# Build both binaries
build:
	go build -o bin/minidrive-server ./cmd/server
	go build -o bin/minidrive ./cmd/cli

# Run API server
run:
	go run ./cmd/server

# Run tests
test:
	go test ./...

# Run migrations
migrate:
	goose -dir migrations postgres "$(DATABASE_URL)" up

# Seed test data
seed:
	psql "$(DATABASE_URL)" -f scripts/seed.sql

# Reset DB (drop + migrate + seed)
reset:
	./scripts/reset.sh

# Clean build artifacts
clean:
	rm -rf bin/
```

### Binary output

```
bin/
  minidrive-server    # API server
  minidrive           # CLI client
```

Both are single static binaries. Deploy `minidrive-server` to EC2. Distribute `minidrive` to users.

---

## Dependency Graph

```
cmd/server ──► handler ──► storage ──► model
     │              │──► s3client        │
     │              │──► auth            │
     │              │──► acl ──► storage │
     │              │──► model           │
     ├──► config                         │
     ├──► storage                        │
     ├──► s3client                       │
     ├──► cleanup ──► storage            │
     │              ──► s3client         │
     └──► auth                           │

cmd/cli ──► config
     ├──► s3client
     ├──► model
     └──► auth (for token storage)
```

No circular dependencies. `model` is a leaf package — it depends on nothing. Everything else depends downward.
