# MiniDrive — Real-World Follow-Up Questions

Interview-style follow-ups on design decisions. Each question is something an interviewer would ask after seeing the HLD.

---

## PostgreSQL: Single Instance vs Production Setup

### Q: You said single Postgres. What would production look like?

RDS Multi-AZ deployment. AWS maintains a synchronous standby replica in a different availability zone. If the primary fails, RDS automatically promotes the standby — failover takes 1-2 minutes with no data loss because the standby is synchronously replicated. On top of that:

- **Automated daily snapshots** — point-in-time recovery up to 35 days
- **Read replicas** — asynchronous replicas in the same or different regions for read-heavy workloads
- **Connection pooling** — PgBouncer or RDS Proxy in front, so the DB isn't overwhelmed by connection churn from horizontally scaled API instances

### Q: Why would you need multiple instances?

Two different reasons:

1. **Availability** — the Multi-AZ standby handles failover. If the primary dies, traffic switches to the standby. Without it, Postgres down = entire system down.
2. **Read scaling** — sync polling (`GET /changes`) is the highest-QPS read path. Every connected client polls every few seconds. At 100M users with even 1% online, that's 1M clients polling. The primary can't handle that alone. Read replicas absorb the sync read load.

Writes (uploads, commits, permission changes) are lower volume and stay on the primary.

### Q: How do you ensure no replica lag or stale reads?

You can't eliminate replica lag — asynchronous replication always has some delay (typically milliseconds, but can spike to seconds under load). You manage it:

- **Write path** — always hits the primary. No stale reads for the writer because they read-after-write from the primary.
- **Read path for sync** — can tolerate small lag. If a sync response is 500ms behind, the client catches up on the next poll. The cursor-based design handles this naturally — the client just gets the update on the next cycle.
- **Read path for ACLs** — this is sensitive. A revoked permission must not be served from a stale replica. Options:
  - Route ACL checks to the primary (simple, small volume)
  - Use synchronous replication for the standby and route ACL reads there (higher latency on writes)
  - Cache ACLs in Redis with explicit invalidation on permission changes
- **Monitoring** — track `ReplicaLag` CloudWatch metric. Alert if it exceeds a threshold (e.g., 5 seconds).

### Q: How would you shard Postgres at scale?

Shard by `user_id`. Every table already has an `owner_id` or can be traced back to one. This means:

- All of a user's files, folders, versions, chunks, and shares live on the same shard
- No cross-shard joins for single-user operations (which is the common case)
- Sharing is the exception — a shared file's metadata lives on the owner's shard, and the grantee's shard needs to know about it. Options:
  - Async replication of share records to the grantee's shard
  - A lightweight global lookup service that maps share → owner shard
  - Denormalize: copy shared file metadata to the grantee's shard (simplest, eventual consistency)

Shard routing: hash(user_id) % N, or use Citus (Postgres extension for distributed tables).

### Q: What about connection scaling? Each API instance opens connections to Postgres.

This is a real problem. Postgres handles ~500 connections well, degrades after that. With 50 horizontally scaled API pods each opening a pool of 20 connections, you're at 1000 connections.

Solutions:
- **PgBouncer** — connection pooler that sits between API and Postgres. Multiplexes thousands of application connections onto a small number of actual Postgres connections. Transaction-mode pooling works best.
- **RDS Proxy** — AWS managed equivalent of PgBouncer. Integrates with IAM auth, handles failover transparently.
- **Smaller pool per instance** — tune pool size down (5-10 per API pod) and let the pooler handle queuing.

### Q: If the primary goes down mid-transaction, what happens to in-flight writes?

The transaction is lost. The client gets an error and must retry. This is why idempotent API design matters:

- Upload init: returns the same file ID if called twice with the same idempotency key
- Commit: re-verifying already-committed chunks is a no-op
- Metadata updates: version numbers prevent stale writes

The failover to the standby takes 1-2 minutes. During that window, all write operations fail. Clients retry with exponential backoff. No data is corrupted — uncommitted transactions are simply rolled back.

---

## Consistency Model: Cross-Plane Gaps

### Q: What if a user uploads chunks to S3 but never calls commit? How long do orphan chunks live?

The orphan cleanup job runs on a schedule (e.g., every 6 hours). It scans for chunks in S3 that have no corresponding row in `version_chunks` and are older than 24 hours. The 24-hour grace period avoids deleting chunks from an upload that's still in progress (large file on a slow connection).

At scale, this job can't scan all of S3 — that's billions of keys. Instead:
- Track pending uploads in a `pending_uploads` table with a `created_at` timestamp
- The cleanup job queries for pending uploads older than 24h, fetches their chunk list, deletes from S3, then deletes the pending row
- If the client never called init (rogue PUT to S3 with a guessed presigned URL), the URL expires in 15 minutes and the chunk is unreachable. S3 lifecycle rules can auto-delete objects in a `tmp/` prefix after 48 hours as a second safety net.

### Q: You hash chunks with SHA-256. What about hash collisions?

SHA-256 has a 2^256 keyspace. The probability of a collision with 250 billion chunks is roughly 2^(-218) — astronomically unlikely. For comparison, you're more likely to have every S3 server spontaneously fail simultaneously.

If you're still paranoid (some security-critical systems are):
- Store chunk size alongside hash. A collision would need matching hash AND matching size.
- On commit, do a byte-level comparison for chunks that already exist (expensive, only worth it for financial/medical data).

In practice, no production system using SHA-256 content-addressing has ever hit a collision. Git uses SHA-1 (weaker) and only got a theoretical collision via a deliberate attack.

### Q: Two users upload the same file at the same time. What happens at the chunk level?

Both clients compute the same SHA-256 hashes. Both get presigned URLs for the same S3 keys. Two things can happen:

1. **First upload finishes, second starts** — second PUT overwrites the same key with identical bytes. Idempotent. No issue.
2. **Both upload simultaneously** — S3 handles concurrent PUTs to the same key. One wins. The content is identical, so it doesn't matter who wins.

At commit time, both commits reference the same chunk hashes. `ref_count` is incremented for each version. Both versions are valid.

---

## Failure Modes: Deep Dives

### Q: What if S3 is down for 30 minutes? What's the user experience?

- **Uploads fail** — client can't PUT chunks. The API can still issue presigned URLs (that's just a signing operation, no S3 call), but the client's PUTs to S3 will fail. Client should show "storage unavailable, will retry" and queue uploads locally.
- **Downloads fail** — presigned GET URLs return errors. Client shows cached/local version if available.
- **Metadata still works** — users can browse folders, rename files, manage shares, view versions. Everything that doesn't touch S3.
- **Sync still works** — change feed is metadata-only. Clients see that files changed, but can't download the new content until S3 recovers.

This is the advantage of the two-plane split — a blob plane outage degrades functionality but doesn't take down the whole system.

### Q: What if the API is under a DDoS attack?

Layers of defense:
- **AWS Shield Standard** — included free, handles common L3/L4 attacks
- **ALB rate limiting** — drop connections above a threshold per source IP
- **Application-level rate limiting** — per-user token bucket (e.g., 100 requests/minute). Implemented in middleware using Redis as the counter store.
- **CloudFront** — if the web UI is behind CloudFront, AWS WAF rules can block malicious patterns before they hit the API
- **Auth wall** — most endpoints require a valid JWT. Unauthenticated endpoints (signup, login) are the attack surface. Rate limit these aggressively (5 attempts/minute per IP).

### Q: A user reports their file is corrupted after download. How do you investigate?

Debugging path:
1. Get the file's `version_id` from the user's client logs
2. Look up `version_chunks` — get the ordered list of chunk hashes
3. For each chunk, compare the hash stored in Postgres against the actual SHA-256 of the bytes in S3
4. If hashes match: corruption happened on the client side (disk, RAM, buggy reassembly). Check the client's local hash verification.
5. If hashes don't match: S3 returned different bytes than what was stored. Check S3 data integrity (S3 checksums feature, enabled since 2022). This has never happened in practice.
6. If a chunk is missing: ref_count bug — a chunk was deleted while still referenced. Check the cleanup job logs.

The commit-time hash verification is the first line of defense. If the commit passed, the data was correct at upload time.

### Q: What happens if the cleanup job accidentally deletes a chunk that's still referenced?

Data loss for any version referencing that chunk. This is a critical bug, not a graceful failure. Mitigation:

- **Double-check before delete** — the cleanup job queries `version_chunks` for the hash before deleting. Only delete if ref_count = 0 AND no rows reference it.
- **Soft-delete chunks too** — don't actually remove from S3 immediately. Mark as `pending_delete`, wait 7 days, then hard delete. This gives a recovery window.
- **Immutable ref_count** — use Postgres transactions. Decrement ref_count and check for zero in the same transaction. No race between decrement and delete.
- **Monitoring** — alert if any download returns a 404 from S3 for a committed version. This should never happen.

---

## Capacity & Scaling: Follow-Ups

### Q: You estimated 100K sync polls/sec. How would you reduce that without WebSockets?

Before jumping to WebSockets (which add operational complexity):

1. **Adaptive polling** — if the last N polls returned no changes, increase the interval (10s → 30s → 60s). When a change is detected, drop back to 10s. This alone can reduce load by 5-10x for idle users.
2. **Long polling** — client makes a GET request, server holds it open for up to 30 seconds. Returns immediately if there's a change, otherwise returns empty after timeout. Cuts request count dramatically. More connection-friendly than short polling.
3. **Server-Sent Events (SSE)** — one-way push over HTTP. Simpler than WebSockets (no upgrade handshake, works through proxies). Good middle ground.
4. **Batch changes** — instead of one change per response, return batches of up to 100 changes. Reduces round trips for users who are catching up.

Scaling order: short polling → adaptive polling → long polling → SSE → WebSocket.

### Q: Your metadata DB is 20 TB at 100M users. How do you handle migrations at that scale?

Schema migrations on a 20 TB database are dangerous. A naive `ALTER TABLE ADD COLUMN` locks the table for hours.

Strategies:
- **Online schema change tools** — `pg_repack` or `pgroll` for Postgres. They create a shadow table, copy data, swap. No lock.
- **Expand-and-contract pattern** — add new column as nullable (instant, no rewrite). Backfill in batches. Update app to write both old and new. Flip reads to new column. Drop old column later.
- **Per-shard migrations** — with sharded Postgres, migrate one shard at a time. If a migration fails, only one shard is affected.
- **Avoid wide-table rewrites** — use JSONB columns for extensible fields (e.g., file metadata). Adding a new attribute doesn't change the schema.

### Q: How would you estimate AWS costs at 100M users?

Ballpark monthly costs at scale:

| Resource | Estimate |
|---|---|
| S3 storage (700 PB) | ~$16M (Standard), ~$4M (Infrequent Access mix) |
| S3 requests (PUTs + GETs) | ~$500K |
| EC2 (20 instances, c6g.xlarge) | ~$20K |
| RDS (16 shards, each db.r6g.2xlarge + replicas) | ~$200K |
| Data transfer (egress) | ~$6M (2.32 GB/s × 2.6M seconds/month × $0.09/GB) |
| Redis (ElastiCache cluster) | ~$30K |
| CloudFront (reduces egress) | Saves ~40% on data transfer |

Total: roughly $10-15M/month. Storage and data transfer dominate. This is why real cloud drives negotiate custom S3 pricing, use tiered storage aggressively, and invest heavily in dedup and delta sync.

### Q: At 250 billion chunks, how does the chunk dedup lookup work without being slow?

The `chunks` table has a primary key on `hash` (SHA-256, 32 bytes). Looking up a hash is a B-tree index scan — O(log n). For 175 billion rows (after dedup):

- B-tree depth: ~5 levels (each level is a page lookup)
- Hot chunks stay in buffer cache
- Cold chunks: 5 random I/Os worst case, ~5ms on SSD

For batch lookups (checking all chunks of a file at commit time): one query with `WHERE hash IN (...)`. Postgres handles this well with index scans.

At extreme scale, you'd put a Bloom filter in front — an in-memory probabilistic structure that tells you "definitely not present" or "maybe present." This avoids the DB round trip for new chunks that are guaranteed to be unique. False positive rate of 0.1% with ~1 GB of RAM for 175 billion entries.

---

## Trade-offs: Deeper Questions

### Q: Why not use a CDN from day one?

CloudFront adds cost and complexity. At MVP (50 users, likely same region), the latency from S3 direct is fine. A CDN helps when:
- Users are geographically distributed (multi-region latency)
- The same chunks are downloaded repeatedly (cache hit rate matters)
- Egress costs are significant

At 50 users, none of these apply. Add CloudFront when download latency complaints appear or egress costs become material.

### Q: Why bcrypt over argon2 for password hashing?

Both are fine. The HLD says bcrypt because:
- `golang.org/x/crypto/bcrypt` is in Go's extended stdlib — no third-party dependency
- bcrypt is battle-tested for 25 years, widely understood
- argon2 is newer and technically stronger (memory-hard, resistant to GPU attacks), but Go's argon2 library requires tuning memory/time/thread parameters

For a learning project, bcrypt is simpler and sufficient. In a production system handling real user credentials, argon2id would be the better choice.

### Q: You said presigned URLs expire in 5-15 minutes. How do you choose the expiry?

Trade-off between security and usability:
- **Too short (1 min)** — large file uploads fail because the URL expires before all chunks are uploaded. Client needs to request new URLs mid-upload.
- **Too long (24 hours)** — a leaked URL gives prolonged access to the chunk. Worse for shared files where access might be revoked.
- **Sweet spot for uploads: 15 minutes** — enough for a 4 MB chunk even on a slow connection. If the upload takes longer, something is wrong.
- **Sweet spot for downloads: 5 minutes** — downloads start immediately, and S3 streams the data once the connection is established. The expiry only matters for the initial request.

If a URL expires mid-operation, the client requests a fresh one from the API. The API re-checks permissions before issuing — so if access was revoked in the meantime, the new URL is denied.

### Q: Why not use S3 multipart upload instead of your own chunking?

S3 multipart upload and our chunking solve different problems:

- **S3 multipart** — breaks a single S3 object into parts for reliable upload. The final object is one key. Parts are not individually addressable after completion.
- **Our chunking** — breaks a file into content-addressed chunks that are each a separate S3 object. Enables dedup, delta sync, and independent chunk downloads.

We actually use both: each 4 MB chunk is uploaded as a single S3 PUT (not multipart, since 4 MB is small enough). If we had larger chunk sizes (e.g., 64 MB), we'd use S3 multipart for each chunk.

The key difference: our chunks are the unit of dedup and sync. S3 multipart parts are just a transport mechanism.

---

## API Design: Authentication & Tokens

### Q: Why short-lived access tokens (15 min) with a separate refresh token?

If an access token is stolen (leaked in logs, XSS attack, intercepted), the attacker has a 15-minute window — not unlimited access. The refresh token is:
- Stored server-side (not in the JWT itself), so it can be revoked instantly
- Longer-lived (7 days) so the user doesn't re-login constantly
- Only sent to one endpoint (`/auth/refresh`), reducing exposure surface

Without refresh tokens, you'd either have long-lived JWTs (risky) or force frequent re-login (bad UX).

### Q: How do you revoke a JWT before it expires?

JWTs are stateless — the server doesn't check a database on every request. So you can't truly "revoke" a JWT. Options:

1. **Short expiry + refresh token revocation** — the approach we use. Revoke the refresh token. The access token expires in ≤15 minutes naturally. Acceptable for most cases.
2. **Token blacklist in Redis** — on logout or password change, add the JWT's `jti` (unique ID) to a Redis set with a TTL matching the token's remaining lifetime. Check Redis on every request. Adds latency but gives instant revocation.
3. **Token versioning** — store a `token_version` per user in the DB. Increment it on password change or forced logout. JWT includes the version. Middleware rejects JWTs with an old version. One DB read per request, but cacheable.

For MiniDrive MVP, option 1 is sufficient. A 15-minute window is acceptable for a file storage product. For a banking app, you'd use option 2 or 3.

### Q: Why not use OAuth2 / OIDC instead of DIY auth?

For a real product, you should. OAuth2 gives you:
- Social login (Google, GitHub)
- MFA for free (delegated to the identity provider)
- Standards compliance (OIDC tokens, well-known endpoints)
- Security audit trail from the provider

We're building DIY because it's a learning exercise. The JWT middleware pattern we build is the same pattern you'd use to validate tokens from an OAuth2 provider — so the learning transfers.

---

## API Design: Upload Flow

### Q: Why does the client compute chunk hashes before upload instead of the server computing them after?

Three reasons:

1. **Dedup before transfer** — if the client sends hashes first, the server can respond with `already_exists: true` for chunks that are already in S3. The client skips those uploads entirely. This saves bandwidth and S3 PUT costs.
2. **Integrity verification** — the client knows what it intended to upload. On commit, the server verifies that what's in S3 matches what the client declared. If there's a mismatch, something went wrong in transit.
3. **API never touches bytes** — the server can't compute hashes because it never sees the file data. Data goes directly to S3 via presigned URLs.

Trade-off: the client must implement SHA-256 hashing. This is trivial in Go (`crypto/sha256` stdlib) and every other language.

### Q: What if a client lies about chunk hashes?

On commit, the server verifies each chunk:
1. Check that the S3 object exists at the expected key
2. Optionally verify the S3 object's checksum (S3 supports SHA-256 checksums since 2022)

If a client sends a hash that doesn't match the uploaded content, the commit fails with `422`. The client can't poison another user's chunks because chunk keys are content-addressed — a fake hash points to a key that either doesn't exist or contains different (correct) content from another upload.

### Q: How does resume work after a client crash mid-upload?

1. Client restarts and calls `POST /files` again with the same file name, size, and chunk hashes
2. Server finds the existing `upload_id` in UPLOADING state (matched by parent_id + name + chunk hashes)
3. Server checks S3 for which chunks are already present
4. Returns the same `upload_id` with `already_exists: true` for completed chunks and fresh presigned URLs for missing ones
5. Client uploads only the missing chunks, then calls commit

The `Idempotency-Key` header makes this cleaner — same key returns the same upload_id without the server guessing intent from file attributes.

### Q: What happens if a presigned URL expires before the client uses it?

The client gets a `403 Forbidden` from S3. Recovery:
1. Client calls `POST /files` again (or a dedicated `POST /files/{file_id}/resume` if we add one)
2. Server returns fresh presigned URLs for chunks that haven't been uploaded yet
3. Client continues

This is why presigned URLs are 15 minutes — long enough for normal uploads, short enough that expiry isn't a security concern.

### Q: Can two users upload the same file at the same time and cause issues?

No. Content-addressed storage makes this safe:
- Same content → same SHA-256 → same S3 key
- Two concurrent PUTs to the same S3 key with identical content: both succeed, one overwrites the other with the same bytes. Idempotent.
- At commit time, each user creates their own version row in Postgres pointing to the shared chunks. `ref_count` is incremented for each version.

The chunks don't "belong" to a user — they're shared blobs. Ownership is tracked in the metadata plane.

---

## API Design: Sharing & Permissions

### Q: How does ACL inheritance work for nested folders?

When checking permission for a node:
1. Check `shares` table for a direct share on this node
2. If none found, walk up the parent chain checking each ancestor
3. First match wins (closest ancestor's share applies)

Example: user shares `/Documents` with Sam as editor. Sam can access `/Documents/Reports/Q4.pdf` — the permission is inherited from the folder.

At scale, this tree walk is expensive. Optimization:
- **Materialized permissions** — when a share is created, write explicit permission rows for all descendants. Update on share revoke or folder move.
- **Cache resolved permissions** — Redis cache keyed by `(user_id, node_id)`, invalidated on share changes.
- **Denormalized column** — store `effective_acl` on each node, updated via trigger. Fast reads, slower writes.

MVP: walk the tree. It's at most 10 levels deep (PRD constraint). At scale: materialized permissions + cache.

### Q: What if someone shares a folder, then moves a file into it? Does the new file inherit the share?

Yes. Permission is resolved at access time by walking the tree, not at share-creation time. Moving a file into a shared folder automatically gives the grantee access because the permission check walks up to the shared parent.

This also means moving a file OUT of a shared folder removes the grantee's access (unless the file or its new parent has a separate share).

### Q: Can you share a file inside a folder that's already shared with a different role?

Yes. Direct shares override inherited shares. Example:
- `/Documents` shared with Sam as **viewer**
- `/Documents/secret.pdf` shared with Sam as **editor**
- Sam can edit `secret.pdf` but only view everything else in Documents

The most specific (closest to the node) share wins.

### Q: What happens when you revoke a share while the grantee is actively downloading?

- Presigned URLs already issued are still valid until they expire (5 minutes for downloads)
- The grantee can finish the in-progress download
- Any new request (new download, folder listing, etc.) will be denied immediately
- The grantee's sync feed will show an `unshare` event, and their client removes the local copy (or marks it as unsynced, depending on client policy)

This is a trade-off: we could invalidate presigned URLs immediately by rotating the S3 signing key, but that would invalidate ALL presigned URLs for ALL users. Not worth it for a 5-minute window.

---

## API Design: Sync & Conflicts

### Q: What happens if the change log grows unbounded?

At scale, the change log is the fastest-growing table. Every file create, update, move, delete, share, and unshare generates a row.

Mitigation:
- **Prune old entries** — if every active client has a cursor beyond sequence N, entries ≤ N can be archived or deleted. Track the minimum active cursor per user.
- **Retention window** — keep 90 days of changes. Clients offline longer than 90 days do a full re-sync instead of incremental.
- **Partition by time** — Postgres table partitioning by month. Drop old partitions cheaply.
- **Separate store at scale** — move the change log to a dedicated system (Kafka, DynamoDB) that handles append-heavy workloads better than Postgres.

### Q: How does the client handle a sync conflict?

Conflict detection: client uploads a new version, but the server's `current_version_id` is different from the client's base version (someone else edited while the client was offline).

Resolution (last-writer-wins + conflicted copy):
1. The server accepts the upload as a new version (last writer wins)
2. The server (or client) detects the divergence and creates a copy: `report (conflicted copy - Sam - 2026-11-14).pdf`
3. Both versions are preserved — the user resolves manually

This matches Dropbox's behavior. Google Drive uses operational transforms for real-time collab, but that's in the "Won't Have" list.

### Q: What if the client's cursor points to a sequence number that's been pruned?

The server returns a `410 Gone` or a special response indicating the cursor is too old:

```json
{
  "error": {
    "code": "CURSOR_EXPIRED",
    "message": "Cursor is older than retention window. Full re-sync required."
  }
}
```

The client then does a full sync: fetches the complete file tree from the API, diffs against local state, and reconciles. The new cursor is set to the latest sequence number.

### Q: A single folder delete with 10,000 children generates 10,000 change log entries. Isn't that expensive?

Yes. Options:

1. **Batch insert** — write all 10,000 entries in a single transaction with a bulk INSERT. Fast in Postgres (milliseconds for 10K rows).
2. **Recursive marker** — instead of one entry per child, write a single `delete_tree` event with the folder ID. The sync client understands this means "delete everything under this folder." Fewer rows, but the client needs more logic.
3. **Background processing** — write the folder delete immediately, queue the child deletions as a background job. The sync feed shows the folder delete first; child deletes trickle in over the next few seconds.

MVP: option 1 (batch insert). It's simple and Postgres handles it fine at our scale. At 100M users, option 2 or 3.

---

## API Design: Rate Limiting & Idempotency

### Q: How do you implement per-user rate limiting?

Token bucket algorithm, backed by Redis:
- Each user has a bucket with capacity 100 and refill rate 100/minute
- On each request, decrement the bucket. If empty, return `429`.
- Redis key: `ratelimit:{user_id}`, TTL 1 minute
- The `X-RateLimit-Remaining` and `X-RateLimit-Reset` headers let clients self-throttle

For auth endpoints (signup/login), rate limit by IP instead of user (since there's no user yet). Stricter: 10/minute per IP.

At scale: rate limiting at the ALB or API Gateway level (AWS WAF rate rules) before requests hit the application.

### Q: How does the Idempotency-Key header work?

1. Client sends a request with `Idempotency-Key: <uuid>`
2. Server checks Redis/Postgres for that key
3. If not seen: process the request, store `{key → response}` with 24h TTL
4. If seen: return the stored response without reprocessing

This protects against:
- Network timeouts where the client doesn't know if the request succeeded
- Client retries after a crash
- Double-clicks in a web UI

Only needed for non-idempotent operations (POST). GET, PUT, DELETE are naturally idempotent.

### Q: What about abuse? A user could upload garbage to fill S3.

Defenses:
- **Storage quota per user** (Should Have in PRD) — e.g., 15 GB free. `POST /files` checks remaining quota before issuing presigned URLs.
- **Rate limiting** — caps how fast a user can upload
- **Upload size validation** — reject `POST /files` if declared `size` exceeds max (5 GB per file per PRD)
- **Orphan cleanup** — even if garbage gets into S3, the cleanup job removes it within 24 hours if never committed
- **Account suspension** — admin tooling to flag and disable accounts

---

## Database Schema: Design Decisions

### Q: Why a single `nodes` table for files and folders instead of separate tables?

Simpler queries. Every operation that works on "a thing in the file tree" — rename, move, delete, share, ACL check — doesn't need to know if it's a file or folder. One table, one query, one index.

Separate tables would mean:
- UNION queries everywhere (list folder contents = files UNION folders)
- Two foreign keys on `shares`, `change_log`, etc. (or a polymorphic `node_type` + `node_id` pattern, which is worse)
- Two sets of indexes to maintain

The `type` column distinguishes them where it matters. Folders ignore `current_version_id`. Files always have one. The CHECK constraint enforces valid types.

Trade-off: folders have nullable columns they never use (`current_version_id`). One nullable column is a trivial cost compared to the query complexity of two tables.

### Q: Why `BIGSERIAL` for change_log.seq instead of UUID?

The sync protocol needs strict ordering. UUIDs are random — you can't say "give me everything after this UUID" without a secondary ordering column. BIGSERIAL gives you:
- Monotonically increasing integers — natural cursor
- Gap-free within a transaction
- Cheap comparisons (`seq > $cursor` is a simple integer comparison, indexed)
- Small storage (8 bytes vs 16 for UUID)

The trade-off: BIGSERIAL is a single sequence generator, which is a bottleneck at extreme write throughput. At 100M users, if the change_log is sharded, each shard has its own sequence — and cross-shard ordering requires a global sequencer (like Twitter's Snowflake) or vector clocks. Not a problem at MVP.

### Q: Why store `ref_count` on the chunks table instead of computing it with COUNT(*)?

`SELECT COUNT(*) FROM version_chunks WHERE chunk_hash = $1` works, but it's expensive when deciding whether to delete a chunk. The cleanup job would need to run this for every candidate chunk — potentially millions of queries.

With `ref_count`:
- Deletion check: `WHERE ref_count = 0` — one index scan, batch operation
- Increment on commit: `UPDATE chunks SET ref_count = ref_count + 1 WHERE hash = $1`
- Decrement on version purge: `UPDATE chunks SET ref_count = ref_count - 1 WHERE hash = $1`

Trade-off: ref_count can drift if there's a bug (increment without matching decrement). Mitigation:
- Wrap increment/decrement in the same transaction as the version_chunks INSERT/DELETE
- Periodic reconciliation job: compare ref_count against actual `COUNT(*)` from version_chunks

### Q: The `nodes` table has a partial unique index. What happens during concurrent inserts?

```sql
CREATE UNIQUE INDEX idx_nodes_parent_name
    ON nodes (parent_id, name)
    WHERE deleted_at IS NULL;
```

Two concurrent transactions try to create a file with the same name in the same folder:
1. Both check — name doesn't exist
2. Both attempt INSERT
3. Postgres serializes at the index level — one succeeds, the other gets a unique violation
4. The losing transaction rolls back and returns `409 CONFLICT`

This is safe without application-level locking. Postgres unique indexes are the concurrency control.

### Q: How do you handle the circular foreign key between `nodes.current_version_id` and `versions.node_id`?

A node points to its current version, and a version points back to its node. You can't INSERT both with foreign key constraints enforced.

Solutions:
1. **Don't enforce `current_version_id` as a foreign key** (our approach). It's nullable and set by the application after the version is committed. The application ensures correctness.
2. **Deferred constraints**: Postgres supports `DEFERRABLE INITIALLY DEFERRED` foreign keys, which are only checked at COMMIT. You can insert both rows in one transaction.
3. **Two-step insert**: insert the node first (current_version_id = NULL), then the version, then UPDATE the node.

We use option 1+3: insert node, insert version, update node. The nullable column avoids the circular dependency.

### Q: Your `change_log` writes a row per user who should see the change. A file shared with 1000 users generates 1000 rows on every edit?

Yes. This is the denormalization trade-off for fast sync reads. The sync query is `WHERE user_id = $1 AND seq > $2` — no joins, no permission checks, just an index scan.

Alternative: write one change_log row per event, and at query time, check if the user has access. This saves write amplification but makes every sync poll run a permission check on every changed node. At 100K sync polls/sec, that's unacceptable.

Mitigation for the write amplification:
- Most files are private (1 user = 1 row). Shared files are the minority.
- Batch INSERT the rows in one transaction
- At extreme scale: fan-out to a separate event system (Kafka) that handles the per-user distribution asynchronously

---

## Auth: Deep Dives

### Q: Why HS256 instead of RS256 for JWT signing?

HS256 uses a shared secret — the same key signs and verifies. RS256 uses a private key to sign and a public key to verify.

For MiniDrive:
- Single service architecture — the same process signs and verifies
- No need to share a public key with other services
- HS256 is faster (~10x) than RS256 for signing and verification
- Simpler key management — one env var vs a key pair

Switch to RS256 when:
- Multiple services need to verify tokens (microservices architecture)
- You don't want every service to have the signing key (security boundary)
- You need to publish a JWKS (JSON Web Key Set) endpoint for third-party verification

### Q: What happens if the JWT_SECRET is compromised?

An attacker can forge any JWT — full access to every account. This is catastrophic.

Response:
1. **Rotate immediately** — deploy a new secret
2. **Accept both old and new** for 15 minutes (max JWT lifetime) so existing valid tokens still work
3. **Drop the old secret** after 15 minutes — all old tokens are now expired anyway
4. **Force-refresh all clients** — return 401 on old tokens, clients use refresh tokens to get new JWTs signed with the new secret
5. **Audit** — check logs for suspicious activity during the compromise window

Prevention:
- Secret in AWS Secrets Manager, not env vars on disk
- Rotate secrets on a schedule (quarterly)
- Monitor for tokens with abnormal claims (user_ids that don't exist, extreme expiry times)

### Q: Why not just use sessions (server-side) instead of JWTs?

Sessions store state on the server (typically in Redis). On every request, the server looks up the session. JWTs are stateless — the server verifies the signature and trusts the claims.

JWTs for MiniDrive:
- **Pro**: no Redis dependency for auth at MVP. The middleware is pure compute.
- **Pro**: horizontally scalable — any API instance can verify any token
- **Pro**: good fit for a CLI client that makes many requests in quick succession

Sessions:
- **Pro**: instant revocation (delete the session)
- **Pro**: no sensitive data in the token (session ID is opaque)
- **Con**: Redis dependency from day one
- **Con**: sticky sessions or shared session store needed for horizontal scaling

For a learning project focused on system design, JWT is the more interesting choice — you deal with the tradeoffs of stateless auth.

### Q: A user changes their email. What happens to existing JWTs that have the old email?

The JWT contains `email` as a convenience claim. If the email is updated in the DB, existing JWTs still have the old email until they expire (15 min).

Options:
1. **Do nothing** — the email in the JWT is for display only. The `sub` (user_id) is what matters for auth. Old email in the JWT is stale but harmless. (Our approach.)
2. **Blacklist old JWTs** — add the `jti` to a Redis blacklist. Force the client to refresh.
3. **Don't put email in the JWT** — fetch it from the DB when needed. One extra query on routes that need the email.

Option 1 is fine for MVP. The email claim is a convenience, not a security control.

### Q: How do you handle concurrent refresh token usage from multiple tabs/devices?

Without rotation (our MVP approach): multiple tabs/devices share the same refresh token. All of them can refresh independently. No conflict.

With rotation (production): each refresh call issues a new refresh token and invalidates the old one. If two tabs refresh simultaneously:
1. Tab A sends refresh token T1 → gets T2
2. Tab B sends refresh token T1 (now invalidated) → 401

This is called "refresh token reuse detection." The server should:
- Detect that an already-used token is being reused
- Treat this as a potential token theft
- Invalidate ALL refresh tokens for that user (nuclear option)
- Force re-login on all devices

This is why we skip rotation at MVP — it adds significant complexity.

---

## Sharing & ACL: Deep Dives

### Q: The recursive CTE walks up 10 levels max. What if someone nests folders deeper than 10?

The PRD says "nested to at least 10 levels." We enforce a max depth at the application level:

```go
func validateDepth(ctx context.Context, db *pgx.Pool, parentID uuid.UUID) error {
    depth := 0
    current := parentID
    for current != uuid.Nil {
        depth++
        if depth > 10 {
            return ErrMaxDepthExceeded
        }
        // walk up
        err := db.QueryRow(ctx,
            "SELECT parent_id FROM nodes WHERE id = $1", current).Scan(&current)
        if err != nil {
            break
        }
    }
    return nil
}
```

This runs on folder creation and file/folder move operations. Rejects anything deeper than 10 levels with a `400 BAD REQUEST`.

Without the limit, a malicious user could create deeply nested folders and make the recursive CTE expensive. The depth cap also keeps the permission resolution CTE bounded.

### Q: What about public/link sharing? The PRD mentions "shareable links with optional expiry" as a Should Have.

Link sharing is different from user-based ACLs. Design:

```sql
CREATE TABLE share_links (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id     UUID NOT NULL REFERENCES nodes(id),
    token       CHAR(64) NOT NULL UNIQUE,     -- random, unguessable
    role        VARCHAR(10) NOT NULL CHECK (role IN ('viewer', 'editor')),
    expires_at  TIMESTAMPTZ,                  -- NULL = no expiry
    created_by  UUID NOT NULL REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

- URL format: `https://minidrive.dev/s/{token}`
- Anyone with the link can access the node at the given role — no account required (or require login, configurable)
- The `token` is a random 256-bit hex string — unguessable, not related to the node ID
- Expiry: after `expires_at`, the link returns 410 Gone
- Revocation: delete the row

This is a separate table, not mixed into `shares`. The permission resolution would add one more check: "does a valid share_link exist for this node or any ancestor?"

Not built at MVP.

### Q: Can an editor delete a file that was shared with them?

Yes. Editors have write + delete permissions. But it's a **soft delete** — the file goes to the **owner's** trash, not the editor's.

Why the owner's trash:
- The owner should be able to restore files deleted by an editor
- The editor doesn't own the file and shouldn't have it in their trash
- This matches Google Drive's behavior

Implementation: soft delete sets `deleted_at` on the node. The node's `owner_id` doesn't change. Trash view queries `WHERE owner_id = $1 AND deleted_at IS NOT NULL`.

### Q: How do you prevent privilege escalation? What if someone crafts a request to share a file they don't own?

Every share/unshare/update endpoint checks ownership explicitly:

```go
// In the share handler
node, err := getNode(ctx, db, req.NodeID)
if err != nil {
    return ErrNotFound
}
if node.OwnerID != userID {
    return ErrForbidden  // only owner can share
}
```

This is not in the middleware — it's in the handler, because it's resource-specific. The middleware only verifies "who are you" (JWT). The handler verifies "can you do this to this resource."

Even if an attacker knows a valid node_id, they can't share it unless they're the owner. The API returns the same `403 FORBIDDEN` whether the node exists or not (prevents node enumeration).

### Q: Two users both have editor access. One renames a file while the other moves it. What happens?

Both operations are metadata updates on the same node. Postgres serializes them:

1. User A: `UPDATE nodes SET name = 'new_name' WHERE id = $1` → succeeds, commits
2. User B: `UPDATE nodes SET parent_id = $2 WHERE id = $1` → succeeds, commits

Both changes apply. The file is renamed AND moved. No conflict — they're updating different columns.

If both try to rename to different names simultaneously:
1. User A's UPDATE acquires a row lock
2. User B's UPDATE blocks until A commits
3. B's UPDATE then applies — B's name wins (last writer wins)

Both writes generate change_log entries. Both users' sync clients see the final state.

This is fine for metadata. For content conflicts (two editors uploading different versions), the conflict resolution creates a "conflicted copy" (see Task 12 — Sync Protocol).

### Q: An owner shares a folder with two users at different roles. User A is editor, User B is viewer. What does B see when listing the folder?

B sees the same file listing as A — the folder contents are identical. The difference is what B can DO:
- B can download files ✅
- B cannot upload, rename, move, or delete ❌

The permission check happens on each action, not on the listing. The listing endpoint only requires `PermRead`, which both roles have.

The client UI should show read-only indicators for viewer-role files, but that's a client-side concern. The API returns the user's effective role on each node in the listing response so the client can render appropriately.

---

*More sections will be added as diagram and protocol documents are completed.*
