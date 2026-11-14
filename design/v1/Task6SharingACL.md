# MiniDrive — Sharing & ACL Model (v1)

## Overview

MiniDrive uses a simple ACL (Access Control List) model with two roles and tree-based inheritance. Every file and folder has an owner, and the owner can grant access to other users.

---

## 1. Roles

| Role | Read | Write | Delete | Share | Description |
|---|---|---|---|---|---|
| **owner** | ✅ | ✅ | ✅ | ✅ | Creator of the file/folder. Implicit — not stored in `shares` table. |
| **editor** | ✅ | ✅ | ✅ | ❌ | Can read, upload new versions, rename, move, delete. Cannot share with others. |
| **viewer** | ✅ | ❌ | ❌ | ❌ | Read-only. Can download and view, nothing else. |

**Only the owner can share or revoke.** Editors and viewers cannot re-share. This keeps the permission model simple and auditable.

---

## 2. Inheritance Model

Shares are set on a node (file or folder). Folder shares **inherit down the tree** to all descendants.

```
/Documents          ← shared with Sam as editor
  /Reports
    Q4.pdf          ← Sam can edit (inherited from /Documents)
    /Drafts
      notes.txt     ← Sam can edit (inherited from /Documents)
  budget.xlsx       ← Sam can edit (inherited from /Documents)
/Photos             ← Sam has NO access
```

**Rules:**
- A share on a folder applies to all files and subfolders within it, recursively
- A **direct share** on a child overrides the inherited share from a parent
- Inheritance is resolved at **access time**, not at share-creation time
- No share rows are created for children — only the explicitly shared node has a row in the `shares` table

### Override Example

```
/Documents          ← shared with Sam as EDITOR
  secret.pdf        ← shared with Sam as VIEWER (direct, overrides parent)
  report.pdf        ← Sam is EDITOR (inherited from /Documents)
```

Sam can edit everything in `/Documents` except `secret.pdf`, which he can only view. The most specific (closest to the node) share wins.

---

## 3. Permission Resolution

When a user requests access to a node, the API resolves permissions by walking up the tree.

### Algorithm

```
function resolvePermission(user_id, node_id):
    // 1. Check ownership
    node = getNode(node_id)
    if node.owner_id == user_id:
        return OWNER

    // 2. Walk up the tree, check for shares at each level
    current = node_id
    while current != null:
        share = SELECT role FROM shares
                WHERE node_id = current AND grantee_id = user_id

        if share exists:
            return share.role    // first match wins (closest ancestor)

        current = getNode(current).parent_id

    // 3. No share found
    return DENIED
```

### SQL Implementation

```sql
-- Get all ancestor IDs for a node (including itself)
WITH RECURSIVE ancestors AS (
    SELECT id, parent_id FROM nodes WHERE id = $1
    UNION ALL
    SELECT n.id, n.parent_id
    FROM nodes n
    JOIN ancestors a ON n.id = a.parent_id
)
-- Find the closest share for this user
SELECT s.role, a.id as node_id
FROM ancestors a
JOIN shares s ON s.node_id = a.id
WHERE s.grantee_id = $2
LIMIT 1;
```

The recursive CTE walks up the tree. The `LIMIT 1` returns the closest (most specific) share. Since the CTE starts from the target node and walks up, the first match is the most specific.

### Performance

- Tree depth is capped at 10 levels (PRD constraint)
- Worst case: 10 rows in the CTE + 10 index lookups on `shares(node_id, grantee_id)`
- At MVP (50 users): sub-millisecond
- At scale: cache resolved permissions in Redis (see Section 7)

---

## 4. Grant Flow

```
Client (Owner)                  API                             PostgreSQL
  │                              │                                │
  │  POST /shares                │                                │
  │  { node_id, grantee_email,   │                                │
  │    role: "editor" }          │                                │
  │─────────────────────────────►│                                │
  │                              │  Auth middleware → get user_id  │
  │                              │                                │
  │                              │  Verify: caller is owner       │
  │                              │  of node_id                    │
  │                              │───────────────────────────────►│
  │                              │◄───────────────────────────────│
  │                              │                                │
  │                              │  If not owner → 403            │
  │                              │                                │
  │                              │  Lookup grantee by email       │
  │                              │───────────────────────────────►│
  │                              │◄───────────────────────────────│
  │                              │                                │
  │                              │  If not found → 404            │
  │                              │  If same as owner → 400        │
  │                              │                                │
  │                              │  BEGIN TRANSACTION             │
  │                              │    INSERT INTO shares          │
  │                              │    (node_id, grantee_id, role) │
  │                              │    ON CONFLICT → 409           │
  │                              │                                │
  │                              │    INSERT INTO change_log      │
  │                              │    (user_id = grantee,         │
  │                              │     node_id, op = 'share')     │
  │                              │  COMMIT                        │
  │                              │───────────────────────────────►│
  │                              │◄───────────────────────────────│
  │                              │                                │
  │  201 { share_id, role }      │                                │
  │◄─────────────────────────────│                                │
```

**Key points:**
- The `change_log` entry is written for the **grantee**, not the owner. This ensures the grantee's sync client picks up the newly shared node.
- If the node is a folder, the grantee's sync client will discover all children on the next sync — no need to write change_log entries for every child.

---

## 5. Revoke Flow

```
Client (Owner)                  API                             PostgreSQL
  │                              │                                │
  │  DELETE /shares/{share_id}   │                                │
  │─────────────────────────────►│                                │
  │                              │  Auth middleware → get user_id  │
  │                              │                                │
  │                              │  Lookup share                  │
  │                              │  Verify: caller is owner       │
  │                              │  of the shared node            │
  │                              │───────────────────────────────►│
  │                              │◄───────────────────────────────│
  │                              │                                │
  │                              │  If not owner → 403            │
  │                              │                                │
  │                              │  BEGIN TRANSACTION             │
  │                              │    DELETE FROM shares           │
  │                              │    WHERE id = $1               │
  │                              │                                │
  │                              │    INSERT INTO change_log      │
  │                              │    (user_id = grantee,         │
  │                              │     node_id, op = 'unshare')   │
  │                              │  COMMIT                        │
  │                              │───────────────────────────────►│
  │                              │◄───────────────────────────────│
  │                              │                                │
  │  204 No Content              │                                │
  │◄─────────────────────────────│                                │
```

**Key points:**
- Revocation is immediate for new requests. Presigned URLs already issued remain valid until expiry (5 min for downloads).
- The `unshare` change_log entry tells the grantee's sync client to remove the node locally.
- Revoking a folder share removes access to ALL descendants — no per-child cleanup needed because inheritance is resolved at access time.

---

## 6. Role Update Flow

```
Client (Owner)                  API                             PostgreSQL
  │                              │                                │
  │  PATCH /shares/{share_id}    │                                │
  │  { role: "viewer" }          │                                │
  │─────────────────────────────►│                                │
  │                              │  Verify: caller is owner       │
  │                              │                                │
  │                              │  UPDATE shares                 │
  │                              │  SET role = $1                 │
  │                              │  WHERE id = $2                 │
  │                              │───────────────────────────────►│
  │                              │◄───────────────────────────────│
  │                              │                                │
  │  200 { share_id, role }      │                                │
  │◄─────────────────────────────│                                │
```

- Downgrading from editor to viewer: grantee loses write access immediately on next request.
- Upgrading from viewer to editor: grantee gains write access immediately.
- No change_log entry needed — the grantee's permissions change, but the file tree doesn't.

---

## 7. Permission Check in Handlers

Every handler that accesses a resource must check permissions. This is NOT in the middleware — it's resource-specific.

### Helper Function

```go
type Permission string

const (
    PermRead  Permission = "read"
    PermWrite Permission = "write"
    PermShare Permission = "share"
)

func CheckPermission(ctx context.Context, db *pgx.Pool, userID, nodeID uuid.UUID, required Permission) error {
    // 1. Check ownership (fast path)
    var ownerID uuid.UUID
    err := db.QueryRow(ctx,
        "SELECT owner_id FROM nodes WHERE id = $1", nodeID).Scan(&ownerID)
    if err != nil {
        return ErrNotFound
    }
    if ownerID == userID {
        return nil // owner can do everything
    }

    // 2. Resolve inherited permission
    role, err := resolveRole(ctx, db, userID, nodeID)
    if err != nil {
        return ErrForbidden
    }

    // 3. Check if role satisfies required permission
    switch required {
    case PermRead:
        // viewer and editor can both read
        return nil
    case PermWrite:
        if role == "editor" {
            return nil
        }
        return ErrForbidden
    case PermShare:
        // only owner can share, and we already checked
        return ErrForbidden
    }

    return ErrForbidden
}
```

### Usage in Handlers

```go
// Download handler
func handleDownload(w http.ResponseWriter, r *http.Request) {
    userID := getUserID(r.Context())
    fileID := getPathParam(r, "file_id")

    if err := CheckPermission(r.Context(), db, userID, fileID, PermRead); err != nil {
        writeError(w, err)
        return
    }
    // ... proceed with download
}

// Upload (new version) handler
func handleCommit(w http.ResponseWriter, r *http.Request) {
    userID := getUserID(r.Context())
    fileID := getPathParam(r, "file_id")

    if err := CheckPermission(r.Context(), db, userID, fileID, PermWrite); err != nil {
        writeError(w, err)
        return
    }
    // ... proceed with commit
}
```

---

## 8. Edge Cases

### Moving a File Into/Out of a Shared Folder

```
BEFORE:
/Documents          ← shared with Sam as editor
  report.pdf        ← Sam can edit (inherited)
/Private            ← not shared

MOVE report.pdf from /Documents to /Private:

AFTER:
/Documents          ← shared with Sam as editor
/Private
  report.pdf        ← Sam CANNOT access (no share on /Private)
```

- Permission is resolved at access time from the current parent chain. Moving a file changes its parent, which changes its inherited permissions.
- The `move` operation writes a change_log entry. Sam's sync client sees the move and removes the file locally.
- The reverse also works: moving a file INTO a shared folder grants the grantee access.

### Deleting a Shared Folder

```
/Documents          ← shared with Sam as editor
  report.pdf
```

Owner deletes `/Documents`:
1. Soft delete — `deleted_at` set on `/Documents` and all children
2. change_log entries written for both owner and Sam
3. Sam's sync client removes everything locally
4. Sam can no longer access any of it (permission check finds the node is deleted)
5. If the owner restores from trash, the share is still in the `shares` table — Sam regains access automatically

### Sharing a Node That's Already Shared With a Parent

```
/Documents          ← shared with Sam as VIEWER
  report.pdf        ← owner now shares with Sam as EDITOR
```

This creates a direct share on `report.pdf` that overrides the inherited viewer role. Sam is editor on `report.pdf` and viewer on everything else in `/Documents`.

If the direct share on `report.pdf` is later revoked, Sam falls back to the inherited viewer role from `/Documents`.

### Sharing With a User Who Already Has Access Via a Parent Share

The API should return `409 CONFLICT` only if there's a direct share on the same node. If the user has access via inheritance, a new direct share on a child is allowed (it's an override, not a duplicate).

```sql
-- This constraint is on the shares table
UNIQUE (node_id, grantee_id)
```

The unique constraint prevents duplicate shares on the same node, but doesn't interfere with inheritance overrides on child nodes.

---

## 9. Shared-With-Me View

The grantee needs a way to see all files shared with them. This is NOT a tree walk — it's a direct query.

```sql
-- All nodes directly shared with this user
SELECT n.id, n.name, n.type, s.role, n.owner_id, u.email as owner_email
FROM shares s
JOIN nodes n ON n.id = s.node_id
JOIN users u ON u.id = n.owner_id
WHERE s.grantee_id = $1
AND n.deleted_at IS NULL
ORDER BY n.name;
```

This returns the top-level shared nodes. The client can then browse into shared folders using the normal folder listing endpoint (which checks permissions via inheritance).

---

## 10. Scaling Considerations

### Problem at Scale

The recursive CTE for permission resolution runs on every request that touches a resource. At 100K QPS, this is expensive.

### Solutions (Not Built at MVP)

**1. Redis Permission Cache**

```
Key:    acl:{user_id}:{node_id}
Value:  "editor" | "viewer" | "denied"
TTL:    5 minutes
```

- On every permission check, look up Redis first
- Cache miss → run the CTE, cache the result
- On share/unshare/move: invalidate all cache entries for the affected node and its descendants

**Invalidation is the hard part.** When a folder share is revoked, every descendant's cache entry for that grantee must be invalidated. Options:
- Scan keys by pattern `acl:{grantee_id}:*` (slow at scale)
- Maintain a reverse index: `node_descendants:{node_id}` → list of descendant IDs
- Accept stale cache for up to 5 min (TTL-based, simpler)

**2. Materialized Permissions Table**

```sql
CREATE TABLE effective_permissions (
    user_id  UUID NOT NULL,
    node_id  UUID NOT NULL,
    role     VARCHAR(10) NOT NULL,
    source_share_id UUID NOT NULL,  -- which share granted this
    PRIMARY KEY (user_id, node_id)
);
```

- On share create: INSERT rows for the node and all descendants
- On share revoke: DELETE rows where source_share_id matches
- On file move: recalculate affected rows
- Permission check becomes a single-row lookup instead of a CTE

Trade-off: writes are much more expensive (a folder share on a folder with 10K children = 10K INSERTs). Reads are O(1). Good for read-heavy workloads (which MiniDrive is).

**3. Hybrid**

- Use the CTE at MVP
- Add Redis cache when latency becomes an issue
- Migrate to materialized permissions only if Redis cache-miss rate is too high

---

## 11. Permission Matrix Summary

| Action | Owner | Editor | Viewer | No Access |
|---|---|---|---|---|
| Download file | ✅ | ✅ | ✅ | ❌ |
| List folder contents | ✅ | ✅ | ✅ | ❌ |
| Upload new version | ✅ | ✅ | ❌ | ❌ |
| Rename / move | ✅ | ✅ | ❌ | ❌ |
| Delete (soft) | ✅ | ✅ | ❌ | ❌ |
| View version history | ✅ | ✅ | ✅ | ❌ |
| Restore version | ✅ | ✅ | ❌ | ❌ |
| Share / revoke | ✅ | ❌ | ❌ | ❌ |
| View shares | ✅ | ❌ | ❌ | ❌ |
| Restore from trash | ✅ | ❌ | ❌ | ❌ |
| Purge from trash | ✅ | ❌ | ❌ | ❌ |
