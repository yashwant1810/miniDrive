# MiniDrive — Upload/Download Flow (v1)

Detailed protocol for how files move between client, API, and S3.

---

## 1. Upload Overview

A file upload has four phases:

```
CLIENT PREP → INIT (API) → CHUNK TRANSFER (S3) → COMMIT (API)
```

- **Client prep**: split file into 4 MB chunks, compute SHA-256 per chunk
- **Init**: client tells the API what it's uploading, API returns presigned URLs
- **Chunk transfer**: client PUTs chunks directly to S3 (API is not in this path)
- **Commit**: client tells API all chunks are in S3, API verifies and commits to Postgres

The API never sees file bytes. It only orchestrates metadata and issues presigned URLs.

---

## 2. Phase 1 — Client Prep

Before any network call, the client processes the file locally.

### Chunking

```go
const ChunkSize = 4 * 1024 * 1024 // 4 MB

func chunkFile(path string) ([]ChunkInfo, error) {
    file, _ := os.Open(path)
    defer file.Close()

    var chunks []ChunkInfo
    buf := make([]byte, ChunkSize)
    index := 0

    for {
        n, err := file.Read(buf)
        if n > 0 {
            hash := sha256.Sum256(buf[:n])
            chunks = append(chunks, ChunkInfo{
                Index: index,
                Hash:  hex.EncodeToString(hash[:]),
                Size:  n,
            })
            index++
        }
        if err == io.EOF {
            break
        }
        if err != nil {
            return nil, err
        }
    }
    return chunks, nil
}
```

**Rules:**
- Every chunk except the last is exactly 4 MB
- The last chunk is 1 byte to 4 MB (whatever remains)
- A file smaller than 4 MB is one chunk
- Empty files (0 bytes): not allowed — reject at the API with `400`

### Why 4 MB?

| Chunk size | Chunks for 1 GB file | Resumability | S3 request overhead |
|---|---|---|---|
| 256 KB | 4,096 | Excellent | Very high (4K PUTs) |
| 4 MB | 256 | Good | Moderate |
| 64 MB | 16 | Poor (lose 64 MB on failure) | Low |

4 MB is S3's minimum part size for multipart upload. It balances resumability (lose at most 4 MB on failure) with request count.

### Hash computation

SHA-256 of each chunk's raw bytes. The hex-encoded digest is 64 characters.

```
chunk content → SHA-256 → hex string → "a3f2b8c1d4e5..."
```

The hash serves three purposes:
1. **Dedup key**: if the hash already exists in the `chunks` table, skip the upload
2. **S3 object key**: the chunk is stored at `ab/cd/abcdef1234...` (first 2 + next 2 chars as prefix)
3. **Integrity verification**: on commit, the API can verify S3 content matches the declared hash

---

## 3. Phase 2 — Init Upload

Client sends metadata + chunk hashes to the API.

### Request

```
POST /files
Authorization: Bearer <jwt>

{
  "parent_id": "folder-uuid",
  "name": "report.pdf",
  "size": 10485760,
  "chunk_hashes": [
    "a3f2b8c1d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1",
    "b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5",
    "c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
  ]
}
```

### What the API does

```
1. Auth middleware → extract user_id from JWT
2. Validate request fields (name not empty, size > 0, hash format valid)
3. Check write permission on parent_id (ACL resolution)
4. Check name uniqueness: SELECT FROM nodes WHERE parent_id = $1 AND name = $2 AND deleted_at IS NULL
5. Check user quota: used_bytes + size <= quota_bytes
6. Check which chunks already exist in the chunks table:

   SELECT hash FROM chunks WHERE hash = ANY($1)

   This returns the set of hashes that are already in S3 (dedup).

7. For each chunk NOT already in S3:
   - Compute the S3 key: ab/cd/abcdef1234...
   - Generate a presigned PUT URL (15 min expiry)

8. Create metadata in a single transaction:
   - INSERT INTO nodes (owner_id, parent_id, name, type='file') → file_id
   - INSERT INTO versions (node_id, status='uploading', size, created_by) → version_id, upload_id

9. Return response
```

### Response

```json
{
  "file_id": "uuid",
  "upload_id": "uuid",
  "chunks": [
    {
      "index": 0,
      "hash": "a3f2b8...",
      "presigned_url": "https://s3.amazonaws.com/bucket/ab/cd/a3f2b8...?X-Amz-...",
      "already_exists": false
    },
    {
      "index": 1,
      "hash": "b4c5d6...",
      "presigned_url": null,
      "already_exists": true
    },
    {
      "index": 2,
      "hash": "c5d6e7...",
      "presigned_url": "https://s3.amazonaws.com/bucket/c5/d6/c5d6e7...?X-Amz-...",
      "already_exists": false
    }
  ]
}
```

- Chunk at index 1 already exists in S3 (dedup). No presigned URL issued. Client skips uploading it.
- Chunks at index 0 and 2 need uploading.

### Presigned URL structure

```
https://{bucket}.s3.{region}.amazonaws.com/{s3_key}
  ?X-Amz-Algorithm=AWS4-HMAC-SHA256
  &X-Amz-Credential=...
  &X-Amz-Date=20261114T120000Z
  &X-Amz-Expires=900
  &X-Amz-Signature=...
```

- `X-Amz-Expires=900` = 15 minutes
- The URL is specific to PUT method — it can't be used for GET or DELETE
- The URL is specific to the exact S3 key — it can't be used for a different object

---

## 4. Phase 3 — Chunk Transfer

Client uploads chunks directly to S3 using the presigned URLs. The API is not involved.

### Client upload logic

```go
func uploadChunks(file *os.File, chunks []ChunkResponse) error {
    // Upload in parallel (bounded concurrency)
    sem := make(chan struct{}, 4) // max 4 concurrent uploads
    var wg sync.WaitGroup
    var uploadErr error

    for _, chunk := range chunks {
        if chunk.AlreadyExists {
            continue // dedup: skip
        }

        wg.Add(1)
        sem <- struct{}{} // acquire slot

        go func(c ChunkResponse) {
            defer wg.Done()
            defer func() { <-sem }() // release slot

            data := readChunk(file, c.Index)

            for attempt := 0; attempt < 3; attempt++ {
                err := httpPut(c.PresignedURL, data)
                if err == nil {
                    return // success
                }
                time.Sleep(backoff(attempt))
            }
            uploadErr = fmt.Errorf("chunk %d failed after 3 retries", c.Index)
        }(chunk)
    }

    wg.Wait()
    return uploadErr
}
```

**Key behaviors:**
- **Parallel uploads**: 4 concurrent chunk PUTs. Saturates bandwidth without overwhelming S3 or the client's network.
- **Retry with backoff**: each chunk retries up to 3 times with exponential backoff (1s, 2s, 4s).
- **Idempotent**: uploading the same chunk twice to the same S3 key overwrites with identical content. Safe.
- **Skip deduped chunks**: chunks with `already_exists: true` are not uploaded.

### S3 PUT details

```
PUT /{s3_key} HTTP/1.1
Host: {bucket}.s3.{region}.amazonaws.com
Content-Length: 4194304
Content-Type: application/octet-stream

<raw chunk bytes>
```

- S3 returns `200 OK` with an `ETag` header (MD5 of the content)
- The client can verify the ETag matches expected MD5, but we rely on SHA-256 at commit time instead

### What if a presigned URL expires?

The client gets `403 Forbidden` from S3.

Recovery:
1. Client calls `POST /files` again with the same parameters and `Idempotency-Key`
2. API returns the same `upload_id` with fresh presigned URLs for chunks that are still missing
3. Client resumes uploading only the missing chunks

### What if the client crashes mid-upload?

Some chunks are in S3, some are not. The version row is in `uploading` status.

On restart:
1. Client calls `POST /files` again with the same file's metadata
2. API finds the existing `uploading` version (matched by parent_id + name or idempotency key)
3. API checks S3 for which chunks are present (HEAD each key)
4. Returns `already_exists: true` for chunks that made it, presigned URLs for the rest
5. Client uploads only the missing chunks, then commits

If the client never comes back, the orphan cleanup job deletes the `uploading` version row and any unreferenced chunks after 24 hours.

---

## 5. Phase 4 — Commit

After all chunks are in S3, the client tells the API to finalize.

### Request

```
POST /files/{file_id}/commit
Authorization: Bearer <jwt>

{
  "upload_id": "uuid"
}
```

### What the API does

```
1. Auth middleware → extract user_id
2. Validate: upload_id belongs to file_id, status is 'uploading', created_by matches user_id
3. Verify chunks in S3:

   For each chunk_hash in the version's declared chunks:
     - HEAD s3://{bucket}/{s3_key}
     - Verify the object exists and size matches expected chunk size
     - (Optional) Verify S3 checksum matches SHA-256

   If any chunk is missing → 422 UNPROCESSABLE ENTITY
     { "error": { "code": "CHUNKS_MISSING", "missing": [0, 2] } }

4. Commit in a single Postgres transaction:

   BEGIN;

   -- Create chunk rows for new chunks (ON CONFLICT for dedup)
   INSERT INTO chunks (hash, size, s3_key, ref_count)
   VALUES ($1, $2, $3, 1)
   ON CONFLICT (hash) DO UPDATE SET ref_count = ref_count + 1;

   -- Create version_chunks mappings
   INSERT INTO version_chunks (version_id, chunk_index, chunk_hash)
   VALUES ($1, $2, $3), ($1, $4, $5), ...;

   -- Update version status
   UPDATE versions SET status = 'committed' WHERE id = $1;

   -- Update node's current version
   UPDATE nodes SET current_version_id = $1, updated_at = now() WHERE id = $2;

   -- Update user's used storage
   UPDATE users SET used_bytes = used_bytes + $size WHERE id = $3;

   -- Write change log entries (one per user with access)
   INSERT INTO change_log (user_id, node_id, op, metadata)
   VALUES ($owner, $node, 'create', '{"name":"report.pdf","version_id":"..."}');
   -- + one row per grantee if the file is in a shared folder

   COMMIT;

5. Return response
```

### Response

```json
{
  "file_id": "uuid",
  "version_id": "uuid",
  "name": "report.pdf",
  "size": 10485760,
  "created_at": "2026-11-14T10:30:00Z"
}
```

### Idempotency

Calling commit twice on the same upload_id:
- First call: runs the full flow, returns `200`
- Second call: detects version is already `committed`, returns the same `200` response without re-executing the transaction

### New version of an existing file

When uploading a new version (file already exists):
- `POST /files` detects the file already exists → creates a new `version` row with `status='uploading'`
- The chunk transfer and commit flow is identical
- On commit, `current_version_id` is updated to the new version
- The old version's chunks remain (for version history). Their `ref_count` stays unchanged.
- If the file is in a shared folder and another user uploaded while this user was uploading, see conflict handling in [Task12SyncProtocol.md](Task12SyncProtocol.md).

---

## 6. Download Flow

Downloads are simpler: one API call for metadata, then parallel GETs from S3.

### Request

```
GET /files/{file_id}/download?version_id=optional
Authorization: Bearer <jwt>
```

### What the API does

```
1. Auth middleware → extract user_id
2. Check read permission (owner, editor, or viewer)
3. Resolve version:
   - If version_id is specified, use that
   - Otherwise, use node.current_version_id
4. Get chunk list:

   SELECT vc.chunk_index, vc.chunk_hash, c.s3_key, c.size
   FROM version_chunks vc
   JOIN chunks c ON c.hash = vc.chunk_hash
   WHERE vc.version_id = $1
   ORDER BY vc.chunk_index;

5. Generate presigned GET URLs for each chunk (5 min expiry)
6. Return response
```

### Response

```json
{
  "file_id": "uuid",
  "version_id": "uuid",
  "name": "report.pdf",
  "size": 10485760,
  "chunks": [
    {
      "index": 0,
      "hash": "a3f2b8...",
      "size": 4194304,
      "presigned_url": "https://s3.amazonaws.com/..."
    },
    {
      "index": 1,
      "hash": "b4c5d6...",
      "size": 4194304,
      "presigned_url": "https://s3.amazonaws.com/..."
    },
    {
      "index": 2,
      "hash": "c5d6e7...",
      "size": 2097152,
      "presigned_url": "https://s3.amazonaws.com/..."
    }
  ]
}
```

### Client download logic

```go
func downloadFile(resp DownloadResponse, destPath string) error {
    outFile, _ := os.Create(destPath)
    defer outFile.Close()

    // Download chunks in parallel, store in temp files
    chunkData := make([][]byte, len(resp.Chunks))
    sem := make(chan struct{}, 4) // max 4 concurrent downloads
    var wg sync.WaitGroup

    for i, chunk := range resp.Chunks {
        wg.Add(1)
        sem <- struct{}{}

        go func(idx int, c ChunkInfo) {
            defer wg.Done()
            defer func() { <-sem }()

            data, _ := httpGet(c.PresignedURL)

            // Verify hash
            hash := sha256.Sum256(data)
            if hex.EncodeToString(hash[:]) != c.Hash {
                panic("chunk hash mismatch — data corrupted in transit")
            }

            chunkData[idx] = data
        }(i, chunk)
    }

    wg.Wait()

    // Reassemble in order
    for _, data := range chunkData {
        outFile.Write(data)
    }

    return nil
}
```

**Key behaviors:**
- **Parallel downloads**: 4 concurrent GETs, same as upload
- **Hash verification**: every chunk's SHA-256 is verified after download. If it doesn't match, the chunk is re-downloaded.
- **Ordered reassembly**: chunks are reassembled by index, not by download completion order
- **Presigned URL expiry**: 5 minutes. Downloads start immediately, so expiry is rarely an issue. If a URL expires, the client requests fresh URLs from the API.

---

## 7. Resume Protocol

Resume handles two scenarios: interrupted uploads and interrupted downloads.

### Upload Resume

```
State: some chunks in S3, version in 'uploading' status, client has file_id + upload_id

1. Client calls POST /files with same params (or a resume endpoint)
2. API recognizes the existing upload (via idempotency key or matching file attributes)
3. API checks S3 for each declared chunk:
   - HEAD s3://{key} → exists? size matches?
4. Returns chunks with already_exists for completed ones, presigned URLs for missing ones
5. Client uploads only missing chunks
6. Client calls commit
```

### Download Resume

```
State: some chunks downloaded to temp files, client knows which are complete

1. Client checks local temp directory for completed chunks
2. For each chunk: verify SHA-256 of local file matches expected hash
3. Request new presigned URLs for missing/corrupt chunks only
4. Download remaining chunks
5. Reassemble
```

The client tracks progress in a local state file:

```json
{
  "file_id": "uuid",
  "version_id": "uuid",
  "chunks": [
    { "index": 0, "hash": "a3f2b8...", "status": "complete" },
    { "index": 1, "hash": "b4c5d6...", "status": "complete" },
    { "index": 2, "hash": "c5d6e7...", "status": "pending" }
  ]
}
```

---

## 8. Presigned URL Security

### Upload URLs (PUT)

| Property | Value |
|---|---|
| Method | PUT only |
| Expiry | 15 minutes |
| Scope | Single S3 key |
| Content | Any bytes (client determines content) |
| Authentication | AWS Signature v4 embedded in URL |

An upload presigned URL cannot be used to:
- Download the object (wrong method)
- Upload to a different key (signature is key-specific)
- Upload after expiry

### Download URLs (GET)

| Property | Value |
|---|---|
| Method | GET only |
| Expiry | 5 minutes |
| Scope | Single S3 key |
| Authentication | AWS Signature v4 embedded in URL |

### URL Leakage

If a presigned URL is leaked (logged, intercepted):
- **Upload URL**: attacker can overwrite the chunk with garbage. But on commit, the API verifies the hash. If content doesn't match, commit fails. The real user retries with fresh URLs.
- **Download URL**: attacker can read the chunk content for up to 5 minutes. This is a data leak for that chunk only. Mitigation: HTTPS everywhere, don't log URLs, short expiry.

### S3 Bucket Policy

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "DenyPublicAccess",
      "Effect": "Deny",
      "Principal": "*",
      "Action": "s3:*",
      "Resource": "arn:aws:s3:::minidrive-chunks/*",
      "Condition": {
        "StringNotEquals": {
          "aws:PrincipalAccount": "123456789012"
        }
      }
    }
  ]
}
```

- Bucket is private. No public access.
- Only the AWS account that owns the bucket can generate presigned URLs.
- S3 Block Public Access is enabled at the account level.
- Server-side encryption (SSE-S3) is enabled — chunks are encrypted at rest.

---

## 9. Deduplication Flow

Content-addressed storage means identical chunks share one S3 object.

### How it works

```
User A uploads file with chunks: [H1, H2, H3]
User B uploads file with chunks: [H2, H4, H5]

Chunk H2 is identical in both files.

Init upload for User B:
  API checks chunks table: H2 exists (ref_count=1)
  Returns: H2 → already_exists: true, no presigned URL
  Returns: H4, H5 → presigned URLs

On commit for User B:
  H2: ref_count incremented to 2
  H4, H5: new rows inserted with ref_count=1

Storage: 5 unique chunks stored, not 6.
```

### Dedup savings

- Two users upload the same 100 MB file: only one copy stored (100% dedup for that file)
- Same user uploads a modified version (10 MB changed): 90% of chunks are reused
- Typical storage-wide dedup ratio: 30-50% for a general file storage service

### Ref count lifecycle

```
Chunk created (first upload):     ref_count = 1
Another version uses same chunk:  ref_count++
Version deleted from trash:       ref_count--
ref_count reaches 0:              chunk eligible for S3 deletion
```

The cleanup job runs periodically and deletes chunks where `ref_count = 0`:

```sql
-- Find orphan chunks
SELECT hash, s3_key FROM chunks WHERE ref_count = 0;

-- For each: delete from S3, then delete from chunks table
DELETE FROM chunks WHERE hash = $1 AND ref_count = 0;
```

The `AND ref_count = 0` in the DELETE guards against a race where a new version references the chunk between the SELECT and DELETE.

---

## 10. Error Handling Summary

| Phase | Error | HTTP | Client action |
|---|---|---|---|
| Init | Invalid fields | 400 | Fix request |
| Init | JWT expired | 401 | Refresh token, retry |
| Init | No write permission | 403 | Show error to user |
| Init | Name conflict | 409 | Prompt user to rename |
| Init | Quota exceeded | 413 | Show error, suggest cleanup |
| Transfer | S3 PUT fails (network) | — | Retry with backoff (3 attempts) |
| Transfer | Presigned URL expired | 403 from S3 | Re-init to get fresh URLs |
| Transfer | S3 unavailable | 503 from S3 | Retry with backoff, show "storage unavailable" |
| Commit | Chunks missing | 422 | Re-upload missing chunks, retry commit |
| Commit | Already committed | 200 (idempotent) | Success — no action needed |
| Commit | Version conflict | 409 | Create conflicted copy (see sync protocol) |
| Download | No read permission | 403 | Show error |
| Download | Version not found | 404 | File may have been deleted |
| Download | Chunk hash mismatch | — | Re-download the chunk |
| Download | Presigned URL expired | 403 from S3 | Request fresh URLs from API |

---

## 11. Flow Diagrams

See [Task8SequenceDiagrams.drawio](Task8SequenceDiagrams.drawio) pages 2 (Chunked Upload) and 3 (Download) for the visual sequence diagrams.

### Upload summary

```
Client                    API                     PostgreSQL              S3
  │                        │                          │                    │
  │  chunk file locally    │                          │                    │
  │  compute SHA-256s      │                          │                    │
  │                        │                          │                    │
  │── POST /files ────────►│                          │                    │
  │                        │── check ACL, quota ─────►│                    │
  │                        │── check existing chunks ►│                    │
  │                        │── create node + version ►│                    │
  │◄── { presigned URLs }──│                          │                    │
  │                        │                          │                    │
  │── PUT chunk 0 ─────────┼──────────────────────────┼───────────────────►│
  │── PUT chunk 2 ─────────┼──────────────────────────┼───────────────────►│
  │   (chunk 1 skipped     │                          │                    │
  │    — already exists)   │                          │                    │
  │◄── 200 OK per chunk ───┼──────────────────────────┼────────────────────│
  │                        │                          │                    │
  │── POST /commit ───────►│                          │                    │
  │                        │── HEAD each chunk ───────┼───────────────────►│
  │                        │◄── all present ──────────┼────────────────────│
  │                        │── BEGIN: write metadata ─►│                    │
  │◄── { version_id } ────│                          │                    │
  │                        │                          │                    │
  │  FILE IS NOW COMMITTED │                          │                    │
```

### Download summary

```
Client                    API                     PostgreSQL              S3
  │                        │                          │                    │
  │── GET /download ──────►│                          │                    │
  │                        │── check ACL ────────────►│                    │
  │                        │── get chunk list ───────►│                    │
  │                        │── generate GET URLs      │                    │
  │◄── { presigned URLs }──│                          │                    │
  │                        │                          │                    │
  │── GET chunk 0 ─────────┼──────────────────────────┼───────────────────►│
  │── GET chunk 1 ─────────┼──────────────────────────┼───────────────────►│
  │── GET chunk 2 ─────────┼──────────────────────────┼───────────────────►│
  │◄── chunk bytes ────────┼──────────────────────────┼────────────────────│
  │                        │                          │                    │
  │  verify SHA-256 each   │                          │                    │
  │  reassemble in order   │                          │                    │
  │  write to local disk   │                          │                    │
```
