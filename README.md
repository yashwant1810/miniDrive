# MiniDrive

A cloud file storage service where users upload, organize, sync, and share files across devices — built from scratch as a system design exercise.

## What is this?

A personal Google Drive clone. The goal is to learn and demonstrate core distributed-systems design by building a real, working product: chunked resumable uploads, folder hierarchy, sharing with ACLs, versioning, and multi-device sync.

This is not a market product. The value is in understanding every layer end to end — enough to whiteboard the full system from memory in a design interview.

## Architecture

Two-plane design:
- **Metadata Plane** (API + PostgreSQL) — handles auth, file tree, permissions, versioning, and sync. Strong consistency.
- **Blob Plane** (S3) — stores file chunks. Clients upload/download directly via presigned URLs. The API never proxies file bytes.

![System Architecture — HLD](design/v1/Task1HLDArchitecture.drawio.png)

## Tech Stack

See [docs/tech-stack.md](docs/tech-stack.md) for full details and reasoning.

| Component    | Choice                         |
| ------------ | ------------------------------ |
| Backend API  | Go (stdlib `net/http`)         |
| CLI Client   | Go                             |
| Web UI       | Next.js (later phase)          |
| Database     | PostgreSQL (RDS)               |
| Blob Storage | S3 (MinIO locally)             |
| Compute      | EC2 t2.micro (free tier)       |
| Auth         | DIY JWT (bcrypt + short-lived) |

## Status

🚧 Phase 0 — Design
