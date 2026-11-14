# MiniDrive — Sync Protocol Design (v1)

## 1. Overview

Sync in MiniDrive means keeping every client's local file tree consistent with the server's authoritative state in PostgreSQL. The server is the single source of truth — clients never talk to each other directly. Every mutation (file create, upload, move, delete, share, unshare) is recorded in an append-only **change log**, and clients consume that log to update their local state.

### The Model

**Cursor-based change feed.** Every change is assigned a monotonically increasing sequence number (`BIGSERIAL`). A client stores the sequence number of the last change it processed — this is its **cursor**. On each poll, the client asks: "give me everything after my cursor." The server returns the next batch of changes and a new cursor.

```
Client                                  Server
  │                                       │
  │  GET /changes?cursor=0&limit=100      │   ← initial sync
  │──────────────────────────────────────►│
  │  { changes: [...], next_cursor: 87,   │
  │    has_more: true }                   │
  │◄──────────────────────────────────────│
  │                                       │
  │  GET /changes?cursor=87&limit=100     │   ← page 2
  │──────────────────────────────────────►│
  │  { changes: [...], next_cursor: 142,  │
  │    has_more: false }                  │
  │◄──────────────────────────────────────│
  │                                       │
  │         ... sleep 10 seconds ...      │
  │                                       │
  │  GET /changes?cursor=142&limit=100    │   ← incremental poll
  │──────────────────────────────────────►│
  │  { changes: [...], next_cursor: 145,  │
  │    has_more: false }                  │
  │◄──────────────────────────────────────│
```

### Why Polling at MVP

Polling is stateless. The server doesn't track who's connected, doesn't manage WebSocket lifecycles, and doesn't handle missed-message recovery. The cursor is just an integer — the client stores it, the server looks it up with a simple indexed query. This eliminates an entire class of bugs (connection management, reconnection, fan-out ordering) while keeping the server dead simple.

At 50 users polling every 10 seconds = **5 queries/second** to Postgres. Trivial.

### Path to Push

At scale (see [HLD Section 5](Task2HLDdoc.md#5-scaling-strategy)), polling becomes expensive — 1M online clients × 1 poll/10s = 100K reads/sec. The upgrade path:

1. **WebSocket/SSE push** — server notifies connected clients immediately when a change is written
2. **Kafka/Redis Pub/Sub** — fan-out bus between API instances and WebSocket servers
3. **Cursor stays** — it becomes the catch-up mechanism for clients that were offline when the push was sent

The cursor-based protocol is the foundation for both polling and push. Push just reduces the latency from ~5s (average poll interval) to near-zero.

---

## 2. Change Log Design

### The `change_log` Table

Defined in the [database schema](Task4DatabaseSchema.md):

```sql
CREATE TABLE change_log (
    seq         BIGSERIAL PRIMARY KEY,
    user_id     UUID NOT NULL REFERENCES users(id),
    node_id     UUID NOT NULL REFERENCES nodes(id),
    op          VARCHAR(20) NOT NULL
                    CHECK (op IN ('create', 'update', 'move', 'delete',
                                  'restore', 'share', 'unshare')),
    metadata    JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_changelog_user_seq ON change_log (user_id, seq);
CREATE INDEX idx_changelog_created_at ON change_log (created_at);
```

- **seq** — `BIGSERIAL` auto-incrementing 64-bit integer. Monotonically increasing, globally ordered. This IS the cursor. A `BIGSERIAL` never wraps (2^63 = 9.2 quintillion — at 1M writes/sec it lasts 292,000 years).
- **user_id** — the user who should see this change. NOT the user who made the change.
- **node_id** — the file or folder affected.
- **op** — the operation type.
- **metadata** — JSONB carrying op-specific data (see below).
- **created_at** — when the change was recorded. Used for pruning, not for sync ordering.

### Operations

| Op | When Written | Description |
|---|---|---|
| `create` | File init + commit, folder creation | A new node appeared in the user's tree |
| `update` | New version committed | File content changed (new version) |
| `move` | Rename or reparent | File/folder moved or renamed |
| `delete` | Soft delete | Node moved to trash |
| `restore` | Trash restore | Node restored from trash |
| `share` | Grant created | User gained access to a node |
| `unshare` | Grant revoked | User lost access to a node |

### Who Gets Change Log Entries

**One row per user who should see the change.** When Alice uploads a new version of a file shared with Bob and Carol, three change_log rows are written:

```sql
INSERT INTO change_log (user_id, node_id, op, metadata) VALUES
    ('alice-uuid', 'file-uuid', 'update', '{"version_id": "v3-uuid", "size": 4194304}'),
    ('bob-uuid',   'file-uuid', 'update', '{"version_id": "v3-uuid", "size": 4194304}'),
    ('carol-uuid', 'file-uuid', 'update', '{"version_id": "v3-uuid", "size": 4194304}');
```

The set of affected users is determined at write time:

```
function getAffectedUsers(node_id):
    owner = SELECT owner_id FROM nodes WHERE id = node_id
    grantees = SELECT grantee_id FROM shares WHERE node_id IN (
        -- node itself + all ancestors (inherited shares)
        WITH RECURSIVE ancestors AS (
            SELECT id, parent_id FROM nodes WHERE id = node_id
            UNION ALL
            SELECT n.id, n.parent_id FROM nodes n
            JOIN ancestors a ON n.id = a.parent_id
        )
        SELECT id FROM ancestors
    )
    return {owner} ∪ grantees
```

### Write Amplification Trade-off

This is a **deliberate denormalization**. Writing N rows (one per user) is more expensive than writing one row and checking permissions at read time. Why we do it:

**If we checked permissions at read time:**
```sql
-- This query would run on every poll, for every user, every 10 seconds
SELECT cl.* FROM change_log cl
JOIN nodes n ON cl.node_id = n.id
WHERE cl.seq > $cursor
AND (
    n.owner_id = $user_id
    OR EXISTS (
        -- Recursive CTE to check inherited permissions
        WITH RECURSIVE ancestors AS (...)
        SELECT 1 FROM ancestors a
        JOIN shares s ON s.node_id = a.id
        WHERE s.grantee_id = $user_id
    )
)
ORDER BY cl.seq LIMIT $limit;
```

This is a join + recursive CTE on every poll. At 5 QPS (50 users) it's fine. At 100K QPS (1M users) it's a disaster.

**With per-user denormalization:**
```sql
-- This is the actual sync query. Fast, indexed, no joins.
SELECT seq, node_id, op, metadata, created_at
FROM change_log
WHERE user_id = $1 AND seq > $2
ORDER BY seq
LIMIT $3;
```

Single table scan on the `(user_id, seq)` index. O(limit) regardless of how many nodes or shares exist. The write amplification (N rows instead of 1) trades write cost for read cost — and sync is overwhelmingly read-heavy.

**Quantifying the amplification:** average file has 1 owner + 2 grantees = 3x write amplification. A file shared with 50 people = 50x. In practice, most files have 0-3 shares. The worst case (large shared folders) is bounded by the number of grantees, not the number of files.

### Metadata JSONB by Operation

Each operation stores different data in the `metadata` field. The client uses this to apply the change locally without making additional API calls.

| Op | Metadata | Example |
|---|---|---|
| `create` | `{ "name", "type", "parent_id", "version_id" }` | `{"name": "report.pdf", "type": "file", "parent_id": "folder-uuid", "version_id": "v1-uuid"}` |
| `update` | `{ "version_id", "size" }` | `{"version_id": "v3-uuid", "size": 8388608}` |
| `move` | `{ "old_parent_id", "new_parent_id", "old_name", "new_name" }` | `{"old_parent_id": "a-uuid", "new_parent_id": "b-uuid", "old_name": "draft.pdf", "new_name": "final.pdf"}` |
| `delete` | `{ }` | `{}` |
| `restore` | `{ "parent_id" }` | `{"parent_id": "folder-uuid"}` |
| `share` | `{ "grantee_id", "role" }` | `{"grantee_id": "bob-uuid", "role": "editor"}` |
| `unshare` | `{ "grantee_id" }` | `{"grantee_id": "bob-uuid"}` |

**Why JSONB and not dedicated columns:** the alternative is a wide table with nullable columns (`old_parent_id`, `new_parent_id`, `old_name`, `new_name`, `version_id`, `grantee_id`, `role`, ...) where most are NULL for any given row. JSONB is cleaner — each op stores only what it needs. The trade-off is no schema enforcement on the JSONB content; the application layer is responsible for writing correct metadata.

### Writing Change Log Entries

Change log writes happen inside the same transaction as the metadata change. This guarantees atomicity — if the metadata change is committed, the change log entry exists; if the transaction rolls back, neither is written.

```sql
-- Example: file commit
BEGIN;
    -- 1. Update the file's current version
    UPDATE nodes SET current_version_id = $version_id, updated_at = now()
    WHERE id = $file_id;

    -- 2. Mark version as committed
    UPDATE versions SET status = 'committed' WHERE id = $version_id;

    -- 3. Write change_log entries for all affected users
    INSERT INTO change_log (user_id, node_id, op, metadata)
    SELECT u.id, $file_id, 'update', $metadata
    FROM (
        -- Owner
        SELECT owner_id AS id FROM nodes WHERE id = $file_id
        UNION
        -- All grantees with access (direct or inherited)
        SELECT DISTINCT s.grantee_id AS id
        FROM shares s
        WHERE s.node_id IN (
            WITH RECURSIVE ancestors AS (
                SELECT id, parent_id FROM nodes WHERE id = $file_id
                UNION ALL
                SELECT n.id, n.parent_id FROM nodes n
                JOIN ancestors a ON n.id = a.parent_id
            )
            SELECT id FROM ancestors
        )
    ) u;
COMMIT;
```

---

## 3. Cursor Mechanics

### What Is a Cursor

A cursor is the `seq` value of the last change the client successfully processed. It's an opaque integer from the client's perspective — the client never interprets it, only stores and sends it back.

```
Timeline:  seq=1  seq=2  seq=3  seq=4  seq=5  seq=6
                                  ▲
                                  │
                          client's cursor = 4
                          "I've seen everything up to 4"
                          "give me 5, 6, ..."
```

### Initial Sync

A brand-new client starts with `cursor = 0`. The first poll returns all changes from the beginning of the user's change log. For a user with many files, this may take multiple pages.

```
GET /changes?cursor=0&limit=100
→ { changes: [seq 1..100], next_cursor: 100, has_more: true }

GET /changes?cursor=100&limit=100
→ { changes: [seq 101..187], next_cursor: 187, has_more: false }

// Client now has full local tree. cursor = 187.
```

### Incremental Sync

After initial sync, the client polls periodically. Most polls return 0-5 changes.

```
GET /changes?cursor=187&limit=100
→ { changes: [seq 188], next_cursor: 188, has_more: false }
```

### The Query

The server executes:

```sql
SELECT seq, node_id, op, metadata, created_at
FROM change_log
WHERE user_id = $1
  AND seq > $cursor
ORDER BY seq ASC
LIMIT $limit;
```

This hits the `idx_changelog_user_seq` composite index — a B-tree range scan starting from `(user_id, cursor)`. Postgres returns results in index order, so no sort is needed at the executor level.

### Response Construction

```go
func handleGetChanges(userID uuid.UUID, cursor int64, limit int) ChangeResponse {
    rows := db.Query(`
        SELECT seq, node_id, op, metadata, created_at
        FROM change_log
        WHERE user_id = $1 AND seq > $2
        ORDER BY seq ASC
        LIMIT $3`,
        userID, cursor, limit+1,  // fetch limit+1 to detect has_more
    )

    changes := rows[:min(len(rows), limit)]
    hasMore := len(rows) > limit

    var nextCursor int64
    if len(changes) > 0 {
        nextCursor = changes[len(changes)-1].Seq
    } else {
        nextCursor = cursor  // no changes — cursor stays the same
    }

    return ChangeResponse{
        Changes:    changes,
        NextCursor: nextCursor,
        HasMore:    hasMore,
    }
}
```

**Key details:**
- **`next_cursor`** = `seq` of the last item returned. If no changes, cursor stays the same.
- **`has_more`** = whether the result was truncated by the limit. Determined by fetching `limit+1` rows and checking if the extra row exists.
- **Pagination:** if `has_more` is true, the client immediately fetches the next page (no 10s wait). This ensures catch-up is fast after a long offline period.
- **Empty response:** cursor stays the same, client waits 10 seconds before polling again.

### Response JSON

```json
{
  "changes": [
    {
      "seq": 188,
      "node_id": "a1b2c3d4-...",
      "node_type": "file",
      "op": "update",
      "name": "report.pdf",
      "parent_id": "e5f6a7b8-...",
      "version_id": "f9e8d7c6-...",
      "metadata": {
        "version_id": "f9e8d7c6-...",
        "size": 8388608
      },
      "timestamp": "2026-11-14T10:30:00Z"
    }
  ],
  "next_cursor": 188,
  "has_more": false
}
```

### Cursor Invariants

1. **Monotonically increasing:** `next_cursor >= cursor` always. Never goes backward.
2. **No gaps for a given user:** within a user's change log, there are no missing entries between two seq values. (There may be seq gaps globally because other users' entries have seq values in between.)
3. **At-least-once delivery:** the client may see the same change twice if it crashes after applying a change but before saving the cursor. The client must handle this idempotently.
4. **Total ordering per user:** all changes for a user are strictly ordered by seq. No ambiguity about which change happened first.

---

## 4. Client Sync Daemon

### Architecture

The sync daemon runs as a background goroutine in the CLI client. It operates independently of user-initiated commands (upload, download, ls) but shares the same auth credentials.

```
┌──────────────────────────────────────────┐
│               CLI Client                  │
│                                           │
│  ┌─────────────┐    ┌──────────────────┐ │
│  │  User CLI    │    │  Sync Daemon     │ │
│  │  (commands)  │    │  (background)    │ │
│  │              │    │                  │ │
│  │  upload      │    │  poll /changes   │ │
│  │  download    │    │  apply locally   │ │
│  │  ls          │    │  save cursor     │ │
│  │  mv / rm     │    │                  │ │
│  └──────┬───────┘    └────────┬─────────┘ │
│         │                     │           │
│         └──────────┬──────────┘           │
│                    │                      │
│           ┌────────▼────────┐             │
│           │  Local State    │             │
│           │  ~/.minidrive/  │             │
│           │  sync_state.json│             │
│           │  credentials    │             │
│           └─────────────────┘             │
└──────────────────────────────────────────┘
```

### Local State

The daemon persists its cursor and sync metadata in a local file:

**`~/.minidrive/sync_state.json`**

```json
{
  "user_id": "a1b2c3d4-...",
  "cursor": 188,
  "last_sync_at": "2026-11-14T10:30:00Z",
  "sync_root": "/Users/alice/MiniDrive",
  "device_id": "d1e2f3a4-..."
}
```

- **cursor** — the `seq` of the last processed change. Survives client restarts.
- **last_sync_at** — informational, for debug and UI display.
- **sync_root** — the local directory that mirrors the server file tree.
- **device_id** — unique per device, generated on first run. Used for multi-device conflict attribution.

File permissions: `0600` (owner-only read/write), same as the credentials file.

### Main Loop

```go
func (d *SyncDaemon) Run(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        default:
        }

        resp, err := d.client.GetChanges(d.cursor, 100)
        if err != nil {
            if isCursorExpired(err) {
                d.fullResync(ctx)
                continue
            }
            // Network error or server error — backoff and retry
            d.backoff(err)
            continue
        }

        for _, change := range resp.Changes {
            if err := d.applyChange(change); err != nil {
                log.Errorf("failed to apply change seq=%d: %v", change.Seq, err)
                // Log and skip — don't block the entire sync loop
                // The file will be inconsistent until a full resync
            }
        }

        d.cursor = resp.NextCursor
        d.saveCursor()

        if !resp.HasMore {
            // Caught up — wait before next poll
            select {
            case <-ctx.Done():
                return
            case <-time.After(10 * time.Second):
            }
        }
        // If has_more, loop immediately — no sleep
    }
}
```

**Key behaviors:**
- **Immediate pagination:** if `has_more` is true, the next request fires immediately. The 10-second sleep only happens when the client is fully caught up.
- **Cursor persistence:** the cursor is saved to disk after each batch, not after each individual change. If the client crashes mid-batch, it re-processes the batch on restart. Changes must be applied **idempotently** (see below).
- **Error handling:** network errors trigger exponential backoff (1s, 2s, 4s, ... capped at 60s). The cursor doesn't advance on error, so no changes are missed.
- **Cursor expired:** if the server returns `410 Gone` / `CURSOR_EXPIRED`, the daemon falls back to full resync (see Section 7).

### Applying Changes Locally

Each operation maps to a local filesystem action:

```go
func (d *SyncDaemon) applyChange(c Change) error {
    switch c.Op {
    case "create":
        return d.applyCreate(c)
    case "update":
        return d.applyUpdate(c)
    case "move":
        return d.applyMove(c)
    case "delete":
        return d.applyDelete(c)
    case "restore":
        return d.applyRestore(c)
    case "share":
        return d.applyShare(c)
    case "unshare":
        return d.applyUnshare(c)
    default:
        log.Warnf("unknown op %q at seq=%d, skipping", c.Op, c.Seq)
        return nil
    }
}
```

| Op | Client Action | Details |
|---|---|---|
| `create` (file) | Create local placeholder, then download content | `GET /files/{id}/download` to get presigned URLs, download chunks, reassemble |
| `create` (folder) | `mkdir` locally | Create the directory at the correct path in the sync root |
| `update` | Download new version, replace local file | Same as create-file, but overwrites existing content. Old local content is replaced. |
| `move` | `os.Rename()` locally | Move/rename the local file or folder to match the new path |
| `delete` | Remove locally (or move to local `.trash/`) | Remove from the synced tree. Optionally keep in a local trash for safety. |
| `restore` | Re-download from server | Fetch the file content and place it at the restored location |
| `share` | Add to local "Shared with Me" directory, download content | Create `~/MiniDrive/Shared with Me/{owner}/{name}` and download |
| `unshare` | Remove local copy | Delete the local copy from "Shared with Me". The file no longer belongs to this user. |

### Idempotent Application

The client may re-process changes after a crash (cursor was saved before the batch completed). Every local action must be safe to repeat:

- **create folder:** `os.MkdirAll()` — succeeds if already exists.
- **create file:** check if file exists with same version_id. If so, skip download.
- **update:** compare local version_id with change's version_id. If already at this version, skip.
- **move:** check if file is already at target path. If so, no-op.
- **delete:** check if file exists before deleting. If already gone, no-op.

The client maintains a lightweight local index (SQLite or JSON) mapping `node_id → local_path + version_id` to support these checks efficiently.

---

## 5. Conflict Detection

### When Conflicts Happen

A conflict occurs when the client has local changes based on a version that is no longer the server's current version. The timeline:

```
Server:  V1 ──────── V2 (Bob uploads) ─────── V3 (Alice's upload lands)
              │
Alice:   V1 ──── edits offline ──── comes online, tries to upload
              │
         base_version = V1
         server's current = V2
         CONFLICT: base ≠ current
```

Alice's client knows its **base version** — the `version_id` it started editing from. When Alice commits, the server checks:

```go
// During commit
currentVersion := getNode(fileID).CurrentVersionID

if req.BaseVersionID != "" && req.BaseVersionID != currentVersion {
    // Conflict detected
    return ConflictError{
        BaseVersion:    req.BaseVersionID,
        CurrentVersion: currentVersion,
    }
}
```

### Two Types of Conflicts

**1. Content conflict** — two users upload different content for the same file.

```
Alice edits report.pdf (base V1) → uploads new content
Bob edits report.pdf (base V1)   → uploads different content
```

Both can't be the "current" version. This is the real conflict — data could be lost. Resolution: conflicted copy (see Section 6).

**2. Metadata conflict** — two users rename the same file, or move it to different folders.

```
Alice renames report.pdf → quarterly-report.pdf
Bob renames report.pdf → q4-report.pdf
```

Resolution: **last writer wins**. The second rename overwrites the first. No conflicted copy is created — metadata conflicts are low-stakes (the file content is unchanged) and a conflicted copy of a rename would be confusing.

### Detection Summary

| Scenario | Detection | Resolution |
|---|---|---|
| Two users edit content, both online | Both commits succeed (different version_ids). Last commit sets `current_version_id`. | Conflicted copy for the non-current version |
| User edits offline, someone else edits while they're away | On upload, `base_version_id ≠ current_version_id` | Conflicted copy |
| Two users rename simultaneously | Both `PATCH /files/{id}` calls succeed (last one wins) | Last writer wins, no conflict copy |
| Two users move file to different folders | Both `PATCH` calls succeed | Last writer wins |
| User deletes a file someone else is editing | Soft delete. Editor can still commit (file exists, just in trash). | Commit succeeds, restores from trash implicitly OR returns 404 — editor must restore first. |

---

## 6. Conflict Resolution — "Conflicted Copy" Strategy

### Design Principle

**No silent data loss.** When two versions of a file's content diverge, both are preserved. The user decides which one to keep. This is the same approach used by Dropbox and OneDrive.

### How It Works

1. **Last writer wins** for `current_version_id`. The most recently committed version becomes the "current" one on the server. This is already the natural behavior — Postgres serializes commits, and the last `UPDATE nodes SET current_version_id = ...` wins.

2. **The displaced version is preserved** as a new, separate file — the **conflicted copy**. It sits next to the original so both users see both versions.

### Naming Convention

```
{original_name} (conflicted copy - {user_email} - {YYYY-MM-DD}).{ext}
```

Examples:
```
report.pdf
report (conflicted copy - alice@example.com - 2026-11-14).pdf

design.fig
design (conflicted copy - bob@example.com - 2026-11-14).fig

notes.txt
notes (conflicted copy - carol@example.com - 2026-11-14).txt
```

If multiple conflicts on the same day:
```
report (conflicted copy - alice@example.com - 2026-11-14).pdf
report (conflicted copy - alice@example.com - 2026-11-14) (2).pdf
```

### Implementation Flow

**Scenario:** Alice and Bob both edit `report.pdf` starting from V1.

```
                Alice                   Server                    Bob
                  │                       │                        │
   editing V1     │                       │         editing V1     │
                  │                       │                        │
                  │  POST /files/{id}/    │                        │
                  │  commit               │                        │
                  │  (base_version=V1,    │                        │
                  │   upload_id=A)        │                        │
                  │──────────────────────►│                        │
                  │                       │  current was V1,       │
                  │                       │  base matches.         │
                  │                       │  Accept. V2=current.   │
                  │  200 OK (version=V2)  │                        │
                  │◄──────────────────────│                        │
                  │                       │                        │
                  │                       │  change_log: 'update'  │
                  │                       │  (for Alice AND Bob)   │
                  │                       │                        │
                  │                       │◄───────────────────────│
                  │                       │  POST /files/{id}/     │
                  │                       │  commit                │
                  │                       │  (base_version=V1,     │
                  │                       │   upload_id=B)         │
                  │                       │                        │
                  │                       │  current is V2,        │
                  │                       │  base is V1.           │
                  │                       │  CONFLICT DETECTED.    │
                  │                       │                        │
                  │                       │  1. Accept Bob's upload │
                  │                       │     as V3 (last writer │
                  │                       │     wins). V3=current. │
                  │                       │                        │
                  │                       │  2. Create conflict    │
                  │                       │     copy file:         │
                  │                       │     "report (conflict- │
                  │                       │     ed copy - bob -    │
                  │                       │     2026-11-14).pdf"   │
                  │                       │     pointing to V2's   │
                  │                       │     chunks             │
                  │                       │                        │
                  │                       │  3. Write change_log:  │
                  │                       │     - 'update' on      │
                  │                       │       report.pdf (V3)  │
                  │                       │     - 'create' for the │
                  │                       │       conflict copy    │
                  │                       │                        │
                  │                       │  200 OK (version=V3,   │
                  │                       │  conflict=true)        │
                  │                       │──────────────────────►│
```

### Server-Side Conflict Resolution Logic

```go
func commitVersion(ctx context.Context, tx pgx.Tx, fileID, uploadID, baseVersionID, uploaderID uuid.UUID) (*CommitResult, error) {
    // 1. Get current version
    var currentVersionID uuid.UUID
    err := tx.QueryRow(ctx,
        "SELECT current_version_id FROM nodes WHERE id = $1",
        fileID,
    ).Scan(&currentVersionID)
    if err != nil {
        return nil, err
    }

    // 2. Commit the new version (always succeeds — last writer wins)
    newVersionID := commitUpload(ctx, tx, uploadID)  // marks version as committed
    updateCurrentVersion(ctx, tx, fileID, newVersionID)

    result := &CommitResult{
        VersionID: newVersionID,
        Conflict:  false,
    }

    // 3. Check for conflict
    if baseVersionID != uuid.Nil && baseVersionID != currentVersionID {
        // Conflict: base ≠ current. The displaced version is currentVersionID.
        conflictFile := createConflictCopy(ctx, tx, fileID, currentVersionID, uploaderID)
        result.Conflict = true
        result.ConflictCopyID = conflictFile.ID

        // Change log entries for the conflict copy
        writeChangeLogForAffectedUsers(ctx, tx, conflictFile.ID, "create", conflictFile.Metadata)
    }

    // 4. Change log entries for the update
    writeChangeLogForAffectedUsers(ctx, tx, fileID, "update", map[string]interface{}{
        "version_id": newVersionID,
        "size":       newVersion.Size,
    })

    return result, nil
}
```

### Alternative: Client-Side Conflict Resolution

Instead of the server creating the conflict copy, the **client** detects the conflict and handles it:

1. Client tries to commit with `base_version_id = V1`
2. Server returns `409 Conflict` with `current_version_id = V2`
3. Client uploads its content as a new version (V3 becomes current — last writer wins)
4. Client creates the conflict copy locally and uploads it as a new file
5. Both files appear on the server and sync to all devices

**Trade-off:** client-side is simpler for the server but relies on the client being well-behaved. If the client crashes between step 3 and 4, the displaced version (V2) is still on the server as a non-current version — not lost, but not visible as a conflict copy. Server-side resolution is more reliable.

**MiniDrive's choice:** server-side. The server handles conflict resolution in a single transaction, guaranteeing both the update and the conflict copy are atomic.

---

## 7. Offline Behavior

### Short Offline (Minutes to Hours)

The client goes offline, comes back. The cursor is still valid — the change log entries haven't been pruned.

```
            cursor=500
              │
Goes offline  │   Changes happen on server (seq 501-600)
              │
Comes online  │
              │
  GET /changes?cursor=500&limit=100
  → 100 changes, has_more maybe
  Client processes them, catches up.
```

Nothing special needed. The normal sync loop handles it.

### Long Offline (Days to Weeks)

Same as above, just more changes to page through. The client might process thousands of changes across many pages, but the protocol handles it — `has_more` drives immediate pagination until caught up.

**Performance concern:** if the user has 50,000 pending changes, the initial catch-up involves 500 pages of 100 changes each. At ~50ms per round-trip, that's ~25 seconds of pure network time, plus download time for new/updated files. The client should show a progress indicator during catch-up.

### Very Long Offline (Beyond Retention Window)

Change log entries are pruned after **90 days** (see Section 9). If a client's cursor points to a seq that's been deleted, the server can't serve incremental changes.

**Server response:**

```json
{
  "error": {
    "code": "CURSOR_EXPIRED",
    "message": "Cursor too old. Change log entries have been pruned. Full resync required.",
    "details": {
      "cursor": 500,
      "oldest_available_seq": 85000
    }
  }
}
```

HTTP status: `410 Gone`

### Full Resync Procedure

When the cursor is expired, the client must rebuild its local state from scratch:

```go
func (d *SyncDaemon) fullResync(ctx context.Context) error {
    log.Info("cursor expired, starting full resync")

    // 1. Get the server's file tree (owned files)
    serverTree, err := d.client.ListFolderRecursive("root")
    if err != nil {
        return err
    }

    // 2. Get shared-with-me files
    sharedNodes, err := d.client.ListSharedWithMe()
    if err != nil {
        return err
    }

    // 3. Build a map of what the server has
    serverMap := buildNodeMap(serverTree, sharedNodes)  // node_id → {path, version_id}

    // 4. Build a map of what the client has locally
    localMap := d.localIndex.GetAll()  // node_id → {path, version_id}

    // 5. Diff: determine what's new, updated, moved, or deleted

    // New on server — download
    for nodeID, serverNode := range serverMap {
        if _, exists := localMap[nodeID]; !exists {
            d.downloadNode(serverNode)
        }
    }

    // Updated on server — download new version
    for nodeID, serverNode := range serverMap {
        if localNode, exists := localMap[nodeID]; exists {
            if localNode.VersionID != serverNode.VersionID {
                d.downloadNode(serverNode)
            }
            if localNode.Path != serverNode.Path {
                os.Rename(localNode.Path, serverNode.Path)
            }
        }
    }

    // Deleted on server — remove locally
    for nodeID, localNode := range localMap {
        if _, exists := serverMap[nodeID]; !exists {
            os.RemoveAll(localNode.Path)
            d.localIndex.Remove(nodeID)
        }
    }

    // 6. Upload local changes that weren't synced before going offline
    d.uploadPendingLocalChanges(ctx)

    // 7. Set cursor to the latest seq
    latestCursor, err := d.client.GetLatestCursor()
    if err != nil {
        return err
    }
    d.cursor = latestCursor
    d.saveCursor()

    log.Info("full resync complete, cursor=%d", d.cursor)
    return nil
}
```

**Step-by-step:**

| Step | Action | API Call |
|---|---|---|
| 1 | Fetch server's complete file tree | `GET /folders/root?recursive=true` (paginated) |
| 2 | Fetch shared files | `GET /shares/incoming` (the shared-with-me list) |
| 3-4 | Build server vs local maps | Local computation |
| 5a | Download new files | `GET /files/{id}/download` for each new file |
| 5b | Download updated files | Same, for files with a different `version_id` |
| 5c | Move/rename changed paths | `os.Rename()` locally |
| 5d | Remove deleted files | `os.RemoveAll()` locally |
| 6 | Upload pending local changes | `POST /files`, `POST /files/{id}/commit` for each |
| 7 | Set cursor to latest | Server returns the current max seq for this user |

### Long Offline + Local Edits

If the user made local changes while offline (edited files, created new files), those changes need to be uploaded **before** processing server changes. Otherwise, server changes might overwrite local work.

```
1. Upload pending local changes (push)
2. Process server changes (pull)
3. Conflicts resolved per Section 6
```

The client tracks pending local changes in `~/.minidrive/pending_changes.json`:

```json
{
  "pending": [
    {
      "node_id": "file-uuid",
      "type": "update",
      "local_path": "/Users/alice/MiniDrive/report.pdf",
      "base_version_id": "v1-uuid",
      "queued_at": "2026-11-14T08:00:00Z"
    },
    {
      "node_id": null,
      "type": "create",
      "local_path": "/Users/alice/MiniDrive/new-doc.txt",
      "parent_id": "folder-uuid",
      "queued_at": "2026-11-14T09:00:00Z"
    }
  ]
}
```

On reconnect, the daemon processes this queue first. Any conflicts that arise are resolved using the conflicted copy strategy from Section 6.

---

## 8. Multi-Device Sync

### Model

A user has multiple devices (laptop, desktop, phone). Each device:
- Has its own sync daemon instance
- Maintains its own cursor (independent of other devices)
- Stores its own `device_id` in `sync_state.json`

The devices never communicate directly. Sync happens entirely through the server:

```
Device A (laptop)           Server              Device B (desktop)
  │                           │                        │
  │  edit report.pdf          │                        │
  │  commit → V2              │                        │
  │──────────────────────────►│                        │
  │                           │  change_log: update    │
  │  200 OK                   │  (seq=501, V2)         │
  │◄──────────────────────────│                        │
  │                           │                        │
  │                           │   (within 10 seconds)  │
  │                           │                        │
  │                           │◄───────────────────────│
  │                           │  GET /changes?cursor=  │
  │                           │  500                   │
  │                           │                        │
  │                           │  { seq=501, op=update, │
  │                           │    version_id=V2 }     │
  │                           │───────────────────────►│
  │                           │                        │
  │                           │        downloads V2    │
  │                           │                        │
```

### Propagation Latency

In the polling model, the worst-case propagation time is:

```
propagation = poll_interval + network_RTT + download_time
            = 10s (max) + ~50ms + varies
            ≈ 10 seconds for small files
```

Average propagation: **~5 seconds** (half the poll interval).

### Simultaneous Edits on Two Devices

This is the most interesting multi-device scenario. Both devices edit the same file at the same time.

```
Timeline:
  Device A: starts editing V1 at t=0
  Device B: starts editing V1 at t=0
  Device A: commits V2 at t=60
  Device B: commits V3 at t=65

Server state after both commits:
  - V1 (old), V2 (Alice's device A), V3 (Alice's device B, now current)
  - current_version_id = V3 (last writer wins)
  - Conflict copy created for V2
```

Both devices see both files on the next sync poll:

**Device A sees:**
- `report.pdf` → V3 (from Device B's edit)
- `report (conflicted copy - alice@example.com - 2026-11-14).pdf` → V2 (Device A's version, displaced)

**Device B sees:**
- Same two files

The user resolves the conflict manually: keep one, merge them, or delete one.

**Same-user conflicts are treated identically to cross-user conflicts.** The server doesn't distinguish between "two devices of the same user" and "two different users." The change log, cursor, and conflict copy mechanisms work the same way. The only difference is the conflict copy name includes the email — for same-user conflicts, both copies show the same email, but the dates/times disambiguate.

### Cursor Independence

Each device has its own cursor. Device A at cursor=500 and Device B at cursor=450 is normal — it just means Device B is slightly behind. There's no coordination needed between devices.

```json
// Device A: ~/.minidrive/sync_state.json
{
  "user_id": "alice-uuid",
  "cursor": 500,
  "device_id": "device-a-uuid"
}

// Device B: ~/.minidrive/sync_state.json
{
  "user_id": "alice-uuid",
  "cursor": 450,
  "device_id": "device-b-uuid"
}
```

---

## 9. Edge Cases

### Folder Delete With Children

When a folder is deleted, all children are soft-deleted recursively. Each deleted node gets its own change_log entry.

```sql
-- Recursive soft delete
WITH RECURSIVE subtree AS (
    SELECT id FROM nodes WHERE id = $folder_id
    UNION ALL
    SELECT n.id FROM nodes n
    JOIN subtree s ON n.parent_id = s.id
    WHERE n.deleted_at IS NULL
)
UPDATE nodes SET deleted_at = now()
WHERE id IN (SELECT id FROM subtree);
```

Then, for each deleted node, change_log entries are written for all affected users.

**Client behavior:** the client processes delete events in the order they arrive (by seq). Since the parent's delete event has a lower seq than its children's, the client may try to delete a folder that still has children locally. The client should use `os.RemoveAll()` on the parent OR process deletes in any order (deleting a file that's already gone is a no-op).

**Large folder concern:** deleting a folder with 10,000 children produces 10,000 change_log entries per affected user. If 3 users have access, that's 30,000 rows inserted in one transaction. This is the worst case for write amplification. Mitigation: batch the inserts, or accept the large transaction at MVP.

### Moving a File Into a Shared Folder

```
BEFORE:
/Private/report.pdf          ← only Alice has access
/Shared-Project/              ← shared with Bob as editor

MOVE report.pdf to /Shared-Project/:

AFTER:
/Shared-Project/report.pdf   ← Alice AND Bob have access
```

The server detects that the file's effective permissions changed (new parent has shares that the old parent didn't). It writes:
- A `move` change_log entry for Alice (the owner)
- A `create` change_log entry for Bob (he didn't have access before — from his perspective, a new file appeared)

```sql
-- Users who had access at old location
old_users = getAffectedUsers(old_parent_id)
-- Users who have access at new location
new_users = getAffectedUsers(new_parent_id)

-- Users who gained access: get a 'create' event
gained = new_users - old_users
-- Users who lost access: get a 'delete' event (see next section)
lost = old_users - new_users
-- Users who still have access: get a 'move' event
kept = old_users ∩ new_users
```

### Moving a File Out of a Shared Folder

The reverse of the above. Users who lose access get a `delete` change_log entry — from their perspective, the file disappeared.

```
BEFORE:
/Shared-Project/report.pdf   ← Alice AND Bob have access

MOVE report.pdf to /Private/:

AFTER:
/Private/report.pdf           ← only Alice has access
```

Bob's sync client sees a `delete` event for `report.pdf` and removes it locally.

### Share Revoked While Client Is Offline

```
t=0:   Bob's client goes offline (cursor=100)
t=1h:  Alice revokes Bob's share on /Documents
       → change_log: {user_id: bob, op: 'unshare', node_id: /Documents}
       → seq=150
t=3h:  Bob's client comes back online
       GET /changes?cursor=100
       → sees seq=150, op='unshare'
       → removes /Documents and all its contents from local disk
```

The protocol handles this naturally. The `unshare` event is just another change in the log. No special handling needed beyond the normal event processing.

### Rapid Successive Edits

Alice saves a file 5 times in 2 minutes. Each save creates a new version and a new change_log entry:

```
seq=200: update, version_id=V2
seq=201: update, version_id=V3
seq=202: update, version_id=V4
seq=203: update, version_id=V5
seq=204: update, version_id=V6
```

Bob's sync client processes them in order. For each `update`, it downloads the new version. In practice, this means Bob downloads 5 versions in quick succession, but only the last one (V6) matters for the final local state.

**Optimization (not at MVP):** the client could coalesce consecutive updates to the same `node_id` within a single batch response — only download the latest version. This is a client-side optimization; the change log is unchanged.

```go
// Client-side coalescing pseudo-code
func coalesce(changes []Change) []Change {
    latest := map[uuid.UUID]Change{}
    order := []uuid.UUID{}
    for _, c := range changes {
        if c.Op == "update" {
            if _, seen := latest[c.NodeID]; !seen {
                order = append(order, c.NodeID)
            }
            latest[c.NodeID] = c  // keep the latest
        }
    }
    // Return only the last update per node_id
    result := []Change{}
    for _, id := range order {
        result = append(result, latest[id])
    }
    return result
}
```

### Change Log Pruning

Entries older than **90 days** are eligible for deletion. A background job runs periodically:

```sql
DELETE FROM change_log
WHERE created_at < now() - INTERVAL '90 days';
```

**Why 90 days:** balances storage cost against the likelihood of a client being offline that long. Most clients sync at least once a week. A 90-day window covers extended vacations, lost laptops, and seasonal users.

**Impact on clients:** any client with a cursor older than 90 days will receive `410 Gone` on the next poll and must perform a full resync (Section 7).

**Pruning safety:** the job runs during off-peak hours (e.g., 3 AM UTC). It uses `DELETE ... LIMIT 10000` in a loop to avoid holding a long lock:

```sql
-- Batched pruning to avoid lock contention
DO $$
DECLARE
    deleted_count INT;
BEGIN
    LOOP
        DELETE FROM change_log
        WHERE seq IN (
            SELECT seq FROM change_log
            WHERE created_at < now() - INTERVAL '90 days'
            LIMIT 10000
        );
        GET DIAGNOSTICS deleted_count = ROW_COUNT;
        EXIT WHEN deleted_count = 0;
        PERFORM pg_sleep(0.1);  -- yield to other queries
    END LOOP;
END $$;
```

### Sequence Number Gaps

BIGSERIAL values can have gaps (transaction rollbacks, for example). A client with cursor=100 might see seq=102 as the next change (101 was for a different user, or was rolled back). This is fine — the client doesn't assume contiguous sequences. It just stores whatever `next_cursor` the server returns.

### Duplicate Node Names After Sync

Two devices create a file with the same name in the same folder simultaneously. The second commit fails with `409 Conflict` (unique constraint on `parent_id, name`). The server returns the error, and the client retries with a modified name (e.g., `report (1).pdf`).

This is NOT a sync conflict — it's a create-time name collision handled by the API. The change log only has entries for the files that were successfully created.

---

## 10. Sync Protocol Summary

### Operation → Change Log → Client Action

| Operation | Change Log `op` | Who Gets the Entry | Client Action |
|---|---|---|---|
| File create (upload + commit) | `create` | Owner + all grantees with access | Create local file, download content via presigned URLs |
| File update (new version) | `update` | Owner + all grantees | Download new version, replace local file |
| File move / rename | `move` | Users who have access at old OR new location | `os.Rename()` local file |
| File delete (soft) | `delete` | Owner + all grantees | Remove local file (or move to local trash) |
| File restore (from trash) | `restore` | Owner + all grantees | Re-download file, place at restored path |
| Folder create | `create` | Owner + all grantees (of parent) | `mkdir` locally |
| Folder delete (recursive) | `delete` (one per child) | Owner + all grantees per node | `os.RemoveAll()` on folder |
| Folder move / rename | `move` | Owner + all grantees | `os.Rename()` local folder |
| Share grant | `share` | Grantee only | Add to "Shared with Me", download content |
| Share revoke | `unshare` | Grantee only | Remove local copy from "Shared with Me" |
| Version restore (old → current) | `update` | Owner + all grantees | Download the restored version, replace local file |

### Cursor States

| Client State | Cursor Value | Behavior |
|---|---|---|
| Brand new client | `0` | Full initial sync — pages through entire change history |
| Caught up | Latest seq | Polls every 10s, usually gets empty responses |
| Behind (offline for hours) | Stale but valid | Pages through missed changes, then resumes normal polling |
| Behind (offline > 90 days) | Expired | Server returns `410 Gone`. Client does full resync. |

### Sync Loop State Machine

```
                    ┌──────────────────┐
                    │   IDLE (sleep)    │
                    │  wait 10 seconds  │
                    └────────┬─────────┘
                             │
                             ▼
                    ┌──────────────────┐
                    │      POLL        │
                    │ GET /changes     │
                    │ ?cursor=N        │
                    └────────┬─────────┘
                             │
              ┌──────────────┼──────────────┐
              │              │              │
              ▼              ▼              ▼
     ┌──────────────┐ ┌──────────┐ ┌──────────────┐
     │ has_more=true│ │ empty    │ │ 410 Gone     │
     │ changes      │ │ response │ │ cursor       │
     │ returned     │ │          │ │ expired      │
     └──────┬───────┘ └────┬─────┘ └──────┬───────┘
            │              │              │
            ▼              │              ▼
     ┌──────────────┐      │     ┌──────────────────┐
     │ APPLY CHANGES│      │     │   FULL RESYNC    │
     │ update cursor│      │     │   fetch tree,    │
     │              │      │     │   diff, download, │
     └──────┬───────┘      │     │   upload pending, │
            │              │     │   set new cursor  │
            │              │     └──────┬───────────┘
            ▼              │            │
     ┌──────────────┐      │            │
     │ has_more?    │      │            │
     │ yes → POLL   │──────┘            │
     │ no  → IDLE   │──────────────────►│
     └──────────────┘                   │
                                        │
                    ┌───────────────────►│
                    │                    ▼
                    │           ┌──────────────────┐
                    └───────────│      IDLE        │
                               └──────────────────┘
```

### Error Handling Summary

| Error | HTTP Status | Client Action |
|---|---|---|
| Network unreachable | — | Exponential backoff (1s, 2s, 4s, ... max 60s) |
| Server error | 500 | Retry with backoff |
| Auth expired | 401 | Refresh access token, retry. If refresh fails, re-login. |
| Cursor expired | 410 | Full resync (Section 7) |
| Rate limited | 429 | Wait for `Retry-After` header, then retry |
| Unknown change op | — | Log warning, skip the change, advance cursor |

