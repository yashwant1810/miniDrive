# MiniDrive — API Specification (v1)

Base URL: `https://api.minidrive.dev/v1`

All requests and responses use `Content-Type: application/json` unless noted otherwise.

## Conventions

- **Auth**: all endpoints except signup and login require `Authorization: Bearer <jwt>` header
- **IDs**: UUIDs v4
- **Timestamps**: ISO 8601 (`2026-11-14T10:30:00Z`)
- **Errors**: standard shape for all error responses (see [Error Format](#error-format))
- **Rate limiting**: per-user, 100 requests/minute for authenticated endpoints, 10/minute for auth endpoints. Returns `429` when exceeded.
- **Pagination**: cursor-based where applicable

### Error Format

All error responses follow this shape:

```json
{
  "error": {
    "code": "RESOURCE_NOT_FOUND",
    "message": "File not found",
    "details": {}
  }
}
```

Common error codes used across endpoints:

| HTTP Status | Code | When |
|---|---|---|
| 400 | `INVALID_REQUEST` | Missing/invalid fields, bad format |
| 401 | `UNAUTHORIZED` | Missing, expired, or invalid JWT |
| 403 | `FORBIDDEN` | Valid JWT but no permission for this resource |
| 404 | `RESOURCE_NOT_FOUND` | ID doesn't exist or user can't see it |
| 409 | `CONFLICT` | Duplicate name, version conflict |
| 429 | `RATE_LIMITED` | Too many requests |
| 500 | `INTERNAL_ERROR` | Server error |

---

## Auth

### POST /auth/signup

Create a new account.

**Request Body:**

```json
{
  "email": "user@example.com",
  "password": "min 8 chars"
}
```

**Responses:**

`201 Created`
```json
{
  "user_id": "uuid",
  "email": "user@example.com",
  "created_at": "2026-11-14T10:30:00Z"
}
```

`400` — invalid email format, password too short
`409` — email already registered

---

### POST /auth/login

Authenticate and receive tokens.

**Request Body:**

```json
{
  "email": "user@example.com",
  "password": "string"
}
```

**Responses:**

`200 OK`
```json
{
  "access_token": "jwt",
  "refresh_token": "opaque-token",
  "expires_in": 900
}
```

`401` — invalid email or password

Notes:
- `access_token`: JWT, expires in 15 minutes
- `refresh_token`: opaque token stored server-side, expires in 7 days

---

### POST /auth/refresh

Get a new access token using a refresh token.

**Request Body:**

```json
{
  "refresh_token": "opaque-token"
}
```

**Responses:**

`200 OK`
```json
{
  "access_token": "jwt",
  "expires_in": 900
}
```

`401` — refresh token expired or revoked

---

## Files

### POST /files

Initialize a file upload. Client must compute chunk hashes before calling this.

**Request Body:**

```json
{
  "parent_id": "uuid | null",
  "name": "report.pdf",
  "size": 8388608,
  "chunk_hashes": [
    "sha256-hex-string",
    "sha256-hex-string"
  ]
}
```

- `parent_id`: folder ID, or `null` for user's root
- `size`: total file size in bytes
- `chunk_hashes`: ordered list of SHA-256 hex digests, one per 4 MB chunk

**Responses:**

`201 Created`
```json
{
  "file_id": "uuid",
  "upload_id": "uuid",
  "chunks": [
    {
      "index": 0,
      "hash": "abc123...",
      "presigned_url": "https://s3.amazonaws.com/...",
      "already_exists": false
    },
    {
      "index": 1,
      "hash": "def456...",
      "presigned_url": null,
      "already_exists": true
    }
  ]
}
```

- `already_exists: true` means the chunk is already in S3 (dedup). No upload needed, no presigned URL issued.
- Presigned URLs expire in 15 minutes.

`400` — missing fields, empty chunk list, invalid hash format
`403` — no write permission on parent folder
`409` — file with same name already exists in parent

---

### POST /files/{file_id}/commit

Finalize an upload after all chunks are in S3. API verifies every chunk hash before committing.

**Path Params:** `file_id` (uuid)

**Request Body:**

```json
{
  "upload_id": "uuid"
}
```

**Responses:**

`200 OK`
```json
{
  "file_id": "uuid",
  "version_id": "uuid",
  "name": "report.pdf",
  "size": 8388608,
  "created_at": "2026-11-14T10:30:00Z"
}
```

`400` — upload_id doesn't match file_id
`404` — file_id or upload_id not found
`409` — already committed (idempotent — returns the existing version)
`422` — one or more chunks missing or hash mismatch

Notes:
- Idempotent. Calling commit twice for the same upload returns the same result.
- On success, a `change_log` entry is written and the file becomes visible in sync.

---

### GET /files/{file_id}/download

Get presigned download URLs for all chunks of the current version.

**Path Params:** `file_id` (uuid)

**Query Params:**

| Param | Type | Default | Description |
|---|---|---|---|
| `version_id` | uuid | current | Download a specific version |

**Responses:**

`200 OK`
```json
{
  "file_id": "uuid",
  "version_id": "uuid",
  "name": "report.pdf",
  "size": 8388608,
  "chunks": [
    {
      "index": 0,
      "hash": "abc123...",
      "presigned_url": "https://s3.amazonaws.com/..."
    },
    {
      "index": 1,
      "hash": "def456...",
      "presigned_url": "https://s3.amazonaws.com/..."
    }
  ]
}
```

- Client downloads each chunk, verifies SHA-256, reassembles in order.
- Presigned URLs expire in 5 minutes.

`403` — no read permission
`404` — file not found or version not found

---

### PATCH /files/{file_id}

Rename or move a file.

**Path Params:** `file_id` (uuid)

**Request Body (partial, include only fields to change):**

```json
{
  "name": "new-name.pdf",
  "parent_id": "uuid"
}
```

**Responses:**

`200 OK`
```json
{
  "file_id": "uuid",
  "name": "new-name.pdf",
  "parent_id": "uuid",
  "updated_at": "2026-11-14T10:30:00Z"
}
```

`403` — no edit permission
`404` — file not found
`409` — name already exists in target folder

---

### DELETE /files/{file_id}

Soft delete (move to trash).

**Path Params:** `file_id` (uuid)

**Responses:**

`200 OK`
```json
{
  "file_id": "uuid",
  "deleted_at": "2026-11-14T10:30:00Z"
}
```

`403` — no edit permission (only owner or editor can delete)
`404` — file not found

---

## Folders

### POST /folders

Create a folder.

**Request Body:**

```json
{
  "parent_id": "uuid | null",
  "name": "Documents"
}
```

- `parent_id`: `null` for root-level folder

**Responses:**

`201 Created`
```json
{
  "folder_id": "uuid",
  "parent_id": "uuid | null",
  "name": "Documents",
  "created_at": "2026-11-14T10:30:00Z"
}
```

`400` — missing name
`403` — no write permission on parent
`409` — folder with same name exists in parent

---

### GET /folders/{folder_id}

List contents of a folder.

**Path Params:** `folder_id` (uuid, or `root` for user's root)

**Query Params:**

| Param | Type | Default | Description |
|---|---|---|---|
| `page_size` | int | 50 | Max items per page (max 200) |
| `cursor` | string | null | Cursor from previous response |

**Responses:**

`200 OK`
```json
{
  "folder_id": "uuid",
  "name": "Documents",
  "path": "/Documents",
  "items": [
    {
      "id": "uuid",
      "type": "folder",
      "name": "Photos",
      "created_at": "2026-11-14T10:30:00Z",
      "updated_at": "2026-11-14T10:30:00Z"
    },
    {
      "id": "uuid",
      "type": "file",
      "name": "report.pdf",
      "size": 8388608,
      "current_version_id": "uuid",
      "created_at": "2026-11-14T10:30:00Z",
      "updated_at": "2026-11-14T10:30:00Z"
    }
  ],
  "next_cursor": "string | null"
}
```

`403` — no read permission
`404` — folder not found

---

### PATCH /folders/{folder_id}

Rename or move a folder.

**Path Params:** `folder_id` (uuid)

**Request Body (partial):**

```json
{
  "name": "New Name",
  "parent_id": "uuid"
}
```

**Responses:**

`200 OK`
```json
{
  "folder_id": "uuid",
  "name": "New Name",
  "parent_id": "uuid",
  "updated_at": "2026-11-14T10:30:00Z"
}
```

`400` — moving a folder into itself or its own descendant (circular reference)
`403` — no edit permission
`404` — folder not found
`409` — name conflict in target folder

---

### DELETE /folders/{folder_id}

Soft delete folder and all contents recursively.

**Path Params:** `folder_id` (uuid)

**Responses:**

`200 OK`
```json
{
  "folder_id": "uuid",
  "deleted_at": "2026-11-14T10:30:00Z",
  "items_deleted": 42
}
```

`403` — no edit permission
`404` — folder not found

Notes:
- All children (files and subfolders) are soft-deleted recursively.
- Each deleted item generates a `change_log` entry.

---

## Sharing

### POST /shares

Grant access to a file or folder.

**Request Body:**

```json
{
  "node_id": "uuid",
  "grantee_email": "user@example.com",
  "role": "viewer | editor"
}
```

- `node_id`: file or folder ID
- `role`: `viewer` (read-only) or `editor` (read + write + delete)
- Sharing a folder grants the same role to all children (inherited)

**Responses:**

`201 Created`
```json
{
  "share_id": "uuid",
  "node_id": "uuid",
  "grantee_id": "uuid",
  "role": "viewer",
  "created_at": "2026-11-14T10:30:00Z"
}
```

`400` — invalid role, sharing with yourself
`403` — only the owner can share
`404` — node not found, grantee email not registered
`409` — share already exists (update role with PATCH instead)

---

### PATCH /shares/{share_id}

Update a share's role.

**Path Params:** `share_id` (uuid)

**Request Body:**

```json
{
  "role": "editor"
}
```

**Responses:**

`200 OK`
```json
{
  "share_id": "uuid",
  "role": "editor",
  "updated_at": "2026-11-14T10:30:00Z"
}
```

`403` — only the owner can modify shares
`404` — share not found

---

### DELETE /shares/{share_id}

Revoke access.

**Path Params:** `share_id` (uuid)

**Responses:**

`204 No Content`

`403` — only the owner can revoke
`404` — share not found

Notes:
- Revocation is immediate. Any presigned URLs already issued remain valid until expiry, but the grantee cannot request new ones.

---

### GET /nodes/{node_id}/shares

List all shares for a file or folder.

**Path Params:** `node_id` (uuid)

**Responses:**

`200 OK`
```json
{
  "node_id": "uuid",
  "shares": [
    {
      "share_id": "uuid",
      "grantee_id": "uuid",
      "grantee_email": "user@example.com",
      "role": "viewer",
      "created_at": "2026-11-14T10:30:00Z"
    }
  ]
}
```

`403` — only the owner can view shares
`404` — node not found

---

## Versions

### GET /files/{file_id}/versions

List version history for a file.

**Path Params:** `file_id` (uuid)

**Responses:**

`200 OK`
```json
{
  "file_id": "uuid",
  "versions": [
    {
      "version_id": "uuid",
      "size": 8388608,
      "created_by": "uuid",
      "created_at": "2026-11-14T10:30:00Z",
      "is_current": true
    },
    {
      "version_id": "uuid",
      "size": 7340032,
      "created_by": "uuid",
      "created_at": "2026-11-13T08:00:00Z",
      "is_current": false
    }
  ]
}
```

- Returns last 10 versions, newest first.

`403` — no read permission
`404` — file not found

---

### POST /files/{file_id}/versions/{version_id}/restore

Restore a previous version. Creates a new version with the same chunks as the target version.

**Path Params:** `file_id` (uuid), `version_id` (uuid)

**Responses:**

`201 Created`
```json
{
  "file_id": "uuid",
  "new_version_id": "uuid",
  "restored_from": "uuid",
  "created_at": "2026-11-14T10:30:00Z"
}
```

- Does not delete the current version. The restored version becomes a new entry in history.
- Chunk ref_counts are incremented (same chunks, new version reference).

`403` — no edit permission
`404` — file or version not found

---

## Sync

### GET /changes

Get changes since a cursor position. Used by clients to sync local state.

**Query Params:**

| Param | Type | Default | Description |
|---|---|---|---|
| `cursor` | int64 | 0 | Sequence number to read from (exclusive) |
| `limit` | int | 100 | Max changes to return (max 500) |

**Responses:**

`200 OK`
```json
{
  "changes": [
    {
      "seq": 1001,
      "node_id": "uuid",
      "node_type": "file",
      "op": "create | update | move | delete | restore | share | unshare",
      "name": "report.pdf",
      "parent_id": "uuid",
      "version_id": "uuid | null",
      "timestamp": "2026-11-14T10:30:00Z"
    }
  ],
  "next_cursor": 1050,
  "has_more": false
}
```

- `cursor = 0` returns all changes from the beginning (initial sync).
- `has_more: true` means the client should immediately fetch the next page.
- Client stores `next_cursor` locally and uses it on the next poll.
- Only returns changes the authenticated user can see (own files + shared files).

`401` — invalid token

Notes:
- Changes are ordered by `seq` (monotonically increasing).
- A single user action (e.g., delete folder with 50 children) produces multiple change entries.
- The sync daemon polls this endpoint every 10 seconds.

---

## Trash

### GET /trash

List items in the user's trash.

**Query Params:**

| Param | Type | Default | Description |
|---|---|---|---|
| `page_size` | int | 50 | Max items per page (max 200) |
| `cursor` | string | null | Cursor from previous response |

**Responses:**

`200 OK`
```json
{
  "items": [
    {
      "id": "uuid",
      "type": "file",
      "name": "old-report.pdf",
      "size": 4194304,
      "deleted_at": "2026-11-10T10:30:00Z",
      "purge_at": "2026-12-10T10:30:00Z"
    }
  ],
  "next_cursor": "string | null"
}
```

- `purge_at`: when the item will be permanently deleted (30 days after `deleted_at`).

---

### POST /trash/{node_id}/restore

Restore an item from trash.

**Path Params:** `node_id` (uuid)

**Responses:**

`200 OK`
```json
{
  "node_id": "uuid",
  "restored_to": "uuid | null",
  "name": "old-report.pdf"
}
```

- Restores to original parent if it still exists, otherwise to root.
- If a file with the same name now exists in the original location, the restored file is renamed (e.g., `report (restored).pdf`).

`404` — item not in trash
`409` — original parent was also deleted and not yet restored (restore parent first, or restores to root)

---

### DELETE /trash/{node_id}

Permanently delete an item. Irreversible.

**Path Params:** `node_id` (uuid)

**Responses:**

`204 No Content`

`404` — item not in trash

Notes:
- Decrements chunk `ref_count`. Chunks with `ref_count = 0` are queued for S3 deletion.
- This is the only hard delete in the system.

---

## Common Headers

**Request headers (all endpoints):**

| Header | Required | Description |
|---|---|---|
| `Authorization` | Yes (except signup/login) | `Bearer <jwt>` |
| `Content-Type` | Yes (for POST/PATCH) | `application/json` |
| `Idempotency-Key` | Optional | UUID. Server deduplicates requests with the same key within 24h. Recommended for POST /files and POST /files/{id}/commit. |

**Response headers (all endpoints):**

| Header | Description |
|---|---|
| `X-Request-Id` | UUID for tracing/debugging |
| `X-RateLimit-Remaining` | Requests left in current window |
| `X-RateLimit-Reset` | Unix timestamp when the window resets |
