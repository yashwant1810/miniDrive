# MiniDrive — High-Level Design Document

## 1. System Overview

MiniDrive is split into two planes:

- **Metadata Plane** — a Go API server backed by PostgreSQL. Handles auth, file/folder tree, permissions, versioning, and the sync change log. All metadata operations go through this plane with strong consistency (single Postgres instance, no replication at MVP).

- **Blob Plane** — S3 (MinIO locally). Stores file content as 4 MB chunks keyed by their SHA-256 hash. Clients upload and download chunks directly via presigned URLs issued by the API. The API never touches file bytes.

The client has two paths: a control path to the API (auth, metadata, presigned URL requests) and a data path directly to S3 (chunk transfer). This separation means the API can be small and cheap — it only handles coordination, not bandwidth.

See [architecture diagram](Task1HLDArchitecture.drawio.png) for the visual layout.

## 2. Consistency Model

### Metadata Plane — Strong Consistency

Single PostgreSQL instance at MVP. Every read sees the latest write. No replica lag, no stale reads. In production, this would be an RDS Multi-AZ deployment with automated backups, a standby replica for failover, and read replicas for scaling read-heavy workloads like sync polling (see [RealWorldQuestions.md](../../RealWorldQuestions.md) for deeper discussion).

This gives us:
- **File tree integrity** — unique constraint on (parent_id, name) prevents duplicate names in a folder. No race condition where two clients create the same file.
- **ACL correctness** — permission checks always reflect the latest grant/revoke. No window where a revoked user can still access a file.
- **Version ordering** — versions are strictly ordered per file. No ambiguity about which version is "current".
- **Sync correctness** — the change log sequence numbers are monotonically increasing. A cursor guarantees you see every change exactly once, in order.

Trade-off: single Postgres is a bottleneck and a single point of failure. Acceptable at MVP (50 users). At scale, shard by user_id and add read replicas (see Section 5).

### Blob Plane — Eventually Consistent

S3 is eventually consistent for new objects in rare edge cases, but in practice provides read-after-write consistency (as of Dec 2020, S3 guarantees strong read-after-write). So a chunk is readable immediately after a successful PUT.

The real consistency gap is **cross-plane**: chunks can exist in S3 before the metadata plane knows about them.

### Cross-Plane Consistency

An upload is only "committed" when all three conditions are met:
1. All chunks are uploaded to S3
2. Client calls `POST /files/{id}/commit`
3. API verifies chunk hashes, writes the version row to Postgres

Until commit succeeds, the file does not exist in the system — even if chunks are already in S3.

**What can go wrong between planes:**

| Scenario | State | Resolution |
|---|---|---|
| Chunks uploaded, client crashes before commit | Orphan chunks in S3, no metadata row | Cleanup job: delete chunks older than 24h with no version reference |
| Commit called, Postgres write fails | Chunks in S3, no metadata row | Client retries commit (idempotent). Same orphan cleanup as fallback. |
| Commit succeeds, S3 deletes a chunk (corruption) | Metadata points to missing chunk | S3 durability is 99.999999999% — effectively impossible. If it happens, version is marked corrupt. |
| Two clients upload same file simultaneously | Two sets of chunks in S3, two commit races | Postgres unique constraint on version resolves it — one commit wins, the other retries as a new version |

### What "Committed" Means

A file version transitions through three states:

```
UPLOADING → COMMITTED → (DELETED via soft-delete)
```

- **UPLOADING**: presigned URLs issued, chunks may or may not be in S3 yet. Not visible to other users or sync.
- **COMMITTED**: all chunks verified, metadata row written. Visible to sync, downloads, and sharing.
- **DELETED**: soft-deleted, moved to trash. Chunks retained until trash purge (30 days).

Only COMMITTED versions appear in the change log and sync feed.

## 3. Failure Modes

Every component can fail. The design goal is: no data loss, and the client can always retry safely.

### API Server Failure

| Scenario | Impact | Recovery |
|---|---|---|
| API crashes mid-request | In-flight metadata ops fail. No partial writes — Postgres transactions roll back. | Client retries. API is stateless, restart loses nothing. |
| API crashes during upload orchestration | Presigned URLs already issued remain valid until expiry (5-15 min). Chunks already in S3 stay. Commit never happens. | Client re-inits upload, API returns which chunks are already present, upload resumes. |
| API unreachable (network partition) | No new metadata ops. No new presigned URLs. Existing presigned URLs still work with S3 directly. | Client retries with backoff. Sync pauses until API is back. |

At MVP: single API instance. If it dies, everything waits. At scale: multiple stateless instances behind a load balancer — one dying is invisible to clients.

### PostgreSQL Failure

| Scenario | Impact | Recovery |
|---|---|---|
| Postgres crashes | All metadata operations fail. Uploads in UPLOADING state can't commit. Sync stops. S3 is unaffected. | RDS restarts from latest checkpoint. No committed data lost — WAL ensures durability. |
| Postgres disk full | Writes rejected, reads still work. Sync polling works, new uploads/commits fail. | Alert on 80% disk. Scale storage (RDS allows online resize). |
| Postgres slow (long queries, lock contention) | API latency spikes. Clients time out. | Identify slow queries (pg_stat_statements). Add missing indexes. Kill long-running transactions. |
| RDS failover (Multi-AZ, production) | 1-2 min downtime. In-flight transactions lost. | Clients retry. Idempotent APIs mean retries are safe. Standby takes over with zero data loss. |

Postgres is the single point of failure at MVP. This is the biggest risk. Mitigation: automated RDS snapshots daily, and the entire metadata schema can be rebuilt from S3 chunk inventory as a last resort (slow but possible).

### S3 Failure

| Scenario | Impact | Recovery |
|---|---|---|
| S3 unavailable | Uploads and downloads fail. Metadata ops still work. | Wait. S3 has 99.99% availability SLA. Outages are rare and regional. |
| S3 loses a chunk | Download of that version fails. Other versions unaffected. | 99.999999999% durability makes this near-impossible. If it happens, mark version as corrupt, fall back to previous version. |
| Presigned URL expired | Client gets 403 from S3. | Client requests new presigned URLs from API, retries. |

S3 is the most reliable component in the stack. We don't design around S3 failure — we rely on its durability guarantee.

### Client Failure

| Scenario | Impact | Recovery |
|---|---|---|
| Client crashes mid-upload | Orphan chunks in S3. No commit. | Client restarts, re-inits upload, API returns missing chunks, upload resumes from where it left off. |
| Client crashes mid-download | Partial file on disk. | Client restarts download. Chunks are independently addressable — re-download only what's missing. |
| Client offline for extended period | Sync cursor falls behind. Large backlog of changes on reconnect. | Cursor is persistent. On reconnect, client pages through all missed changes. No data loss, just catch-up time. |
| Client has stale sync state | Local file tree diverges from server. | Full re-sync: client fetches entire file tree from API, diffs against local state, reconciles. |

### Network Failures

| Scenario | Impact | Recovery |
|---|---|---|
| Flaky connection during chunk upload | Some PUTs succeed, some fail. | Client tracks per-chunk status. Retries only failed chunks. Idempotent PUTs — uploading the same chunk twice is harmless (same SHA-256 key). |
| Connection drops between client and API | Metadata request fails. | Client retries with exponential backoff. |
| Connection drops between API and Postgres | API returns 500. | API retries the DB call once. If still failing, returns error to client. Client retries. |
| DNS failure | Nothing resolves. | Out of our control. Client retries with backoff. |

### Concurrent Access Conflicts

| Scenario | Impact | Recovery |
|---|---|---|
| Two users edit the same file simultaneously | Both upload new chunks. Both call commit. | Postgres serializes commits via version sequencing. Both versions are saved. The later one becomes `current_version_id`. No data loss. |
| Two devices for same user edit the same file offline | Both have diverged local copies. Both sync when back online. | First to sync wins as the new version. Second sync detects conflict (server version ≠ local base version), creates a "conflicted copy" file alongside the original. User resolves manually. |
| User deletes a file while another user is downloading it | Soft delete — file moves to trash. Download presigned URLs still work until they expire. | No interrupted download. The file disappears from listings but active transfers complete. |

### Summary: Design Principles for Failure

1. **Idempotent everything** — every API call is safe to retry
2. **Client drives recovery** — the server is stateless; the client tracks upload/download progress and retries
3. **Orphan cleanup as a safety net** — any state that leaks between planes gets cleaned up asynchronously
4. **Soft deletes** — nothing is immediately destroyed, giving a window to recover from mistakes
5. **Fail-open for reads, fail-closed for writes** — if something is degraded, reads (downloads, sync) degrade gracefully; writes (uploads, permission changes) fail explicitly rather than silently corrupting state

## 4. Capacity Estimates (100M Users)

This section is a design exercise — we're not building for 100M users, but we need to be able to talk through the numbers in an interview.

### Assumptions

| Parameter | Value | Rationale |
|---|---|---|
| Total users | 100M | Target scale for analysis |
| Average storage per user | 10 GB | Conservative — Google gives 15 GB free |
| Daily active users (DAU) | 10M (10%) | Typical for a storage product |
| Concurrently online | 1M (1%) | Peak hour estimate |
| Average file size | 2 MB | Mix of docs, images, small videos |
| Chunk size | 4 MB | Design decision from PRD |
| Uploads per active user per day | 5 | Light usage — a few files |
| Downloads per active user per day | 10 | More reads than writes |
| Sync polls per online client | 1 per 10 seconds | PRD target |

### Storage

```
Total storage = 100M users × 10 GB = 1 EB (exabyte)
Total chunks  = 1 EB / 4 MB = 250 billion chunks
```

With content-addressed dedup (same hash stored once), real storage depends on dedup ratio. Conservative estimate: 30% dedup savings (PRD target).

```
Effective storage = 1 EB × 0.7 = 700 PB
```

S3 cost at this scale: ~$23/TB/month × 700,000 TB = ~$16M/month for storage alone. This is why real cloud drives use tiered storage (S3 Infrequent Access, Glacier for old versions).

### Metadata Database Size

| Table | Rows (estimate) | Row size | Total |
|---|---|---|---|
| users | 100M | ~200 bytes | ~20 GB |
| nodes (files + folders) | 5B (50 per user avg) | ~300 bytes | ~1.5 TB |
| versions | 10B (2 versions per file avg) | ~100 bytes | ~1 TB |
| chunks | 175B (after dedup) | ~100 bytes | ~17.5 TB |
| version_chunks | 10B | ~50 bytes | ~500 GB |
| shares | 500M (5 per user avg) | ~100 bytes | ~50 GB |
| change_log | Huge — needs pruning | ~100 bytes | Pruned to 90 days |

Total metadata: ~20 TB. This is too large for a single Postgres instance — confirms the need for sharding at this scale.

### Throughput — Uploads

```
Uploads/day      = 10M DAU × 5 uploads = 50M uploads/day
Uploads/second   = 50M / 86,400 ≈ 580 uploads/sec
Chunks/upload    = avg 2 MB file / 4 MB chunk = 1 chunk (small files don't get chunked, but let's say avg 2 chunks accounting for larger files)
Chunk PUTs/sec   = 580 × 2 ≈ 1,160 PUTs/sec to S3
```

S3 handles this easily — it supports 3,500 PUTs/sec per prefix, and we distribute across many prefixes.

### Throughput — Downloads

```
Downloads/day    = 10M DAU × 10 = 100M downloads/day
Downloads/sec    = 100M / 86,400 ≈ 1,160 downloads/sec
Chunk GETs/sec   = 1,160 × 2 ≈ 2,320 GETs/sec from S3
```

Again, well within S3 limits. A CDN (CloudFront) would reduce this further for hot files.

### Throughput — Metadata API

```
Sync polls       = 1M online clients × 1 poll/10s = 100,000 reads/sec
Upload inits     = 580/sec
Commits          = 580/sec
Folder ops       = ~500/sec (estimate)
Auth checks      = every request = ~102,000/sec
```

**100K reads/sec for sync is the bottleneck.** A single Postgres instance handles ~10K simple queries/sec. This needs:
- Read replicas (10+ replicas for sync reads)
- Redis cache for hot change log entries
- Or move sync to a dedicated store (e.g., Redis sorted sets keyed by user_id)

### Throughput — Bandwidth

```
Upload bandwidth  = 580 uploads/sec × 2 MB avg = 1.16 GB/sec ingest
Download bandwidth = 1,160 downloads/sec × 2 MB avg = 2.32 GB/sec egress
```

All direct to/from S3 — the API never handles this bandwidth. This is why the presigned URL design matters.

### Summary Table

| Metric | Value |
|---|---|
| Total storage | ~700 PB (after dedup) |
| Metadata DB size | ~20 TB (sharded) |
| Chunk PUTs/sec | ~1,160 |
| Chunk GETs/sec | ~2,320 |
| Metadata QPS | ~102,000 (sync-dominated) |
| Upload bandwidth | ~1.16 GB/sec |
| Download bandwidth | ~2.32 GB/sec |

## 5. Scaling Strategy

How we'd evolve from MVP (50 users) to 100M users. None of this is built — it's the roadmap for the design doc and interview discussion.

### API Layer — Horizontal Scaling

MVP: single Go binary on EC2 t2.micro.

At scale:
- **Stateless API instances** behind an ALB (Application Load Balancer). Any instance can handle any request.
- **Auto-scaling group** — scale on CPU and request count. Go's low memory footprint means each instance handles thousands of concurrent connections.
- Estimate: 100K QPS / 5K QPS per instance = ~20 API instances at peak.
- **Health checks** — ALB routes traffic away from unhealthy instances automatically.

No sticky sessions. No local state. JWT auth means every request is self-contained.

### Database — Shard + Read Replicas

MVP: single RDS Postgres instance.

At scale (staged):

**Stage 1: Vertical scaling**
- Upgrade RDS instance class (db.r6g.xlarge → 2xlarge → 4xlarge)
- Gets you to ~5K QPS, maybe 10K with tuning

**Stage 2: Read replicas**
- Add 5-10 read replicas for sync polling
- Route writes to primary, sync reads to replicas
- Application-level routing (separate DB connection pools for read vs write)

**Stage 3: Sharding**
- Shard by `user_id` using hash partitioning
- Each shard is an independent Postgres cluster (primary + replicas)
- Shard router sits between API and DB — maps user_id → shard
- Start with 16 shards, reshard by doubling (consistent hashing)
- Sharing across users: grantee's shard stores a lightweight reference pointing to the owner's shard (see [RealWorldQuestions.md](../../RealWorldQuestions.md))

### Sync — From Polling to Push

MVP: client polls `GET /changes?cursor=N` every 10 seconds.

At scale:
- **WebSocket/SSE connections** — server pushes changes to connected clients instead of clients polling
- Eliminates the 100K reads/sec sync polling load on Postgres
- **Fan-out service** — when a commit or metadata change happens, a background worker publishes the event to all affected users' WebSocket connections
- **Redis Pub/Sub** or **Kafka** as the event bus between API instances and WebSocket servers
- WebSocket servers are stateful (hold connections) but disposable — client reconnects on failure and catches up via cursor

### Blob Storage — S3 Optimizations

MVP: standard S3, all chunks in one bucket.

At scale:
- **S3 Intelligent Tiering** — automatically moves cold chunks to cheaper storage classes
- **S3 Infrequent Access** for versions older than 30 days
- **Glacier** for versions older than 90 days (or deleted files in extended trash)
- **Prefix distribution** — use first 4 chars of chunk hash as prefix (`ab/cd/abcd1234...`) to distribute load across S3 partitions
- **CloudFront CDN** — cache hot chunks at edge locations. Reduces S3 GET costs and improves download latency for popular shared files.
- **Multi-region replication** for disaster recovery (not for latency at first)

### Caching Layer

Not in MVP. At scale:

- **Redis** for:
  - ACL lookups — cache resolved permissions per (user_id, node_id), invalidate on grant/revoke
  - Sync cursors — cache latest change log entries per user, reduces DB reads
  - Session/JWT blacklist — for token revocation before expiry
- **Cache invalidation** — application-level. On every write that affects permissions or metadata, explicitly delete the relevant cache keys. No TTL-based expiry for ACLs (correctness over performance).

### Deduplication at Scale

MVP: content-addressed chunks (SHA-256 hash as key). Same content = same key = stored once.

At scale:
- **Global chunk table** with `ref_count` — tracks how many versions reference each chunk
- When a version is deleted, decrement ref_count. When ref_count = 0, delete from S3.
- **Dedup savings** depend on user behavior. Estimate 30-50% for a general file storage service (many users store the same popular files, OS installers, common documents).
- **Inline dedup** — before issuing presigned URLs, check if chunk hash already exists. If it does, skip the upload entirely. This saves bandwidth and S3 PUT costs.

### Scaling Stages Summary

| Users | API | Database | Sync | Cache | Blob |
|---|---|---|---|---|---|
| 50 (MVP) | 1 × EC2 t2.micro | 1 × RDS db.t3.micro | Polling 10s | None | S3 standard |
| 10K | 2 × EC2 | 1 × RDS larger + 1 read replica | Polling 10s | Redis for ACLs | S3 standard |
| 1M | 5 × EC2 + ALB | Primary + 5 read replicas | WebSocket push | Redis for ACLs + sync | S3 + CloudFront |
| 100M | 20 × EC2 + ALB + ASG | 16 shards × (primary + replicas) | WebSocket + Kafka fan-out | Redis cluster | S3 tiered + CDN + multi-region |

## 6. Trade-offs & Decisions

Every design choice has a trade-off. This section documents what we chose, what we gave up, and why.

### Presigned URLs vs API-Proxied Uploads

**Chose:** clients upload/download directly to S3 via presigned URLs.

**Gave up:** the API has no visibility into transfer progress or bytes. Can't inspect content server-side before it lands in S3 (e.g., virus scanning, content type validation).

**Why:** if the API proxies file data, it becomes a bandwidth bottleneck. At 1.16 GB/sec upload throughput (100M user estimate), you'd need massive API infrastructure just to shuttle bytes. Presigned URLs make the API a lightweight control plane and let S3 handle the heavy lifting — which is exactly what S3 is built for.

**Mitigation for what we gave up:** post-upload validation. After commit, a background job can scan the chunk content if needed.

### 4 MB Chunk Size

**Chose:** fixed 4 MB chunks.

**Gave up:** optimal performance for very small files (a 10 KB file still creates one chunk with overhead) and very large files (4 MB chunks mean a 5 GB file = 1,280 chunks = 1,280 S3 PUTs).

**Why:** balance between resumability and overhead. On a flaky connection, losing a 4 MB chunk means re-uploading 4 MB, not 64 MB. On a fast connection, 4 MB chunks don't create excessive per-request overhead like 256 KB chunks would. 4 MB is also S3's minimum part size for multipart upload.

**Alternative considered:** variable chunk size (CDC — content-defined chunking, like rsync). Better dedup, but significantly more complex. Not worth it for MVP.

### Cursor-Based Polling vs WebSocket Push

**Chose:** polling via `GET /changes?cursor=N` every 10 seconds at MVP.

**Gave up:** real-time sync. A change takes up to 10 seconds to propagate (average 5 seconds).

**Why:** polling is stateless. The server doesn't track who's connected or manage long-lived connections. The cursor is just a sequence number — the client stores it, the server looks it up. This eliminates an entire class of bugs (connection management, reconnection logic, missed messages). At 50 users, the load is trivial.

**At scale:** switch to WebSocket push (see Section 5). The cursor mechanism stays — it becomes the catch-up protocol for clients that were offline.

### Single Postgres vs Distributed Database

**Chose:** single PostgreSQL instance at MVP.

**Gave up:** high availability and horizontal read scaling.

**Why:** strong consistency is simple to reason about when there's one DB. No replica lag, no eventual consistency edge cases, no split-brain. Every read reflects the latest write. For 50 users, a db.t3.micro handles the load easily.

**At scale:** shard by user_id, add read replicas (see Section 5). The application code is designed to make this migration straightforward — every query already filters by user_id or owner_id.

### DIY JWT Auth vs Managed Auth (Cognito/Auth0)

**Chose:** build auth from scratch with bcrypt + short-lived JWTs.

**Gave up:** out-of-the-box features — MFA, social login, password reset flows, account lockout, OIDC compliance.

**Why:** this is a learning project. Auth is a core system design concept. Understanding token issuance, expiry, middleware validation, and refresh flows is part of the goal. A managed service hides all of that.

**In production:** use a managed service. DIY auth is a liability unless you have a dedicated security team.

### Soft Deletes vs Hard Deletes

**Chose:** soft delete everything. Deleted files move to trash, purge after 30 days.

**Gave up:** storage cost — deleted files still consume S3 storage for 30 days. Queries need `WHERE deleted_at IS NULL` everywhere.

**Why:** user safety. Accidental deletes are common. A 30-day trash window matches user expectations (Google Drive, Dropbox both do this). The query overhead is handled by a partial index on `deleted_at IS NULL`.

**Chunk cleanup:** when a version is purged from trash, its chunk ref_counts are decremented. Chunks with ref_count = 0 are deleted from S3 by the cleanup job.

### Content-Addressed Storage vs Random Keys

**Chose:** chunk S3 key = SHA-256 hash of chunk content.

**Gave up:** simplicity — need to compute hashes on the client before upload. Need a global chunk table to track references.

**Why:** dedup for free. If two users upload the same file, the chunks hash to the same keys. The second upload skips entirely (or overwrites with identical data — idempotent). At scale with 100M users, this saves significant storage and bandwidth.

**Security consideration:** two users sharing a chunk hash doesn't leak data. The chunk is only accessible via presigned URLs, which require a valid version reference in the metadata. Knowing a hash doesn't grant access.

---

*End of HLD document. Low-level design documents (API spec, database schema, sequence diagrams) follow as separate design tasks.*
