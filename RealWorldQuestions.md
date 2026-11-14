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

*More sections will be added as LLD documents are completed.*
