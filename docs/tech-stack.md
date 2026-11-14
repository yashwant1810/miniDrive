# MiniDrive — Tech Stack

## Core

| Component    | Choice                          | Why                                                                                              |
| ------------ | ------------------------------- | ------------------------------------------------------------------------------------------------ |
| Backend API  | Go (stdlib `net/http`)          | Learning goal + strong interview signal for storage/infra systems. Stdlib router (Go 1.22+) is enough — no framework needed, which is what interviewers expect. |
| CLI Client   | Go (shared module with backend) | Same language as backend — share models, S3 logic, and auth. Go is a natural fit for CLI tooling. |
| Web UI       | Next.js (Phase 6+)             | Standard React ecosystem, independent of backend. Deferred to final phase.                       |
| Database     | PostgreSQL                      | Strong consistency for the metadata plane (tree structure, ACLs, versions). Mature, free tier eligible on RDS. |
| Blob Storage | S3 (MinIO locally)              | Clients upload/download directly via presigned URLs — API never proxies file bytes. MinIO is S3-compatible for local dev. |
| Compute      | EC2 t2.micro (free tier)        | Simpler than Lambda for long-lived connections and chunked upload orchestration. 750 free hrs/month for 12 months. |
| Auth         | DIY JWT (bcrypt + short-lived)  | Part of the learning goal. Email/password signup, bcrypt hashing, short-lived JWTs with expiry.  |

## Go Libraries

| Purpose    | Library              | Why                                                                                  |
| ---------- | -------------------- | ------------------------------------------------------------------------------------ |
| HTTP       | `net/http` (stdlib)  | Go 1.22+ has method+pattern routing built in. No need for chi/gin/echo.              |
| Postgres   | `pgx` + `sqlc`       | pgx is the fastest Go Postgres driver. sqlc generates type-safe Go from raw SQL — no ORM magic, full control over queries. |
| S3         | `aws-sdk-go-v2`      | Official AWS SDK. Handles presigned URLs, multipart upload, works with MinIO locally. |
| JWT        | `golang-jwt/jwt`     | Most widely used Go JWT library. Simple, well-maintained.                            |
| CLI        | `cobra`              | De facto standard for Go CLIs (used by kubectl, docker, gh).                         |
| Config     | `envconfig`          | Reads config from env vars. Minimal, no magic — fits 12-factor style.                |
| Migrations | `goose`              | SQL-based migrations, no DSL. Runs up/down from CLI or embedded in the binary.       |

## Local Dev

Docker Compose with Postgres and MinIO. Develop and test entirely offline, deploy to AWS by swapping config.

## Deployment

EC2 t2.micro + RDS db.t3.micro + S3 — all within AWS free tier ($0–10/month target).
Single binary deploy to EC2 (no container runtime needed in prod).
