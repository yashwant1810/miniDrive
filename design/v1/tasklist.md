# Design Documents — MiniDrive v1

## High-Level Design

- [ ] 1. System architecture — two-plane overview (metadata + blob), component diagram, data flow, trade-offs
- [ ] 2. HLD document — scaling strategy, consistency model, failure modes, capacity estimates (100M user analysis)

## Low-Level Design

- [ ] 3. API spec — endpoints, request/response schemas, error codes, auth headers, rate limiting
- [ ] 4. Database schema — ER diagram, tables, columns, types, indexes, constraints, relationships
- [ ] 5. Auth flow — signup/login, password hashing, JWT issuance/refresh/expiry, middleware chain
- [ ] 6. Sharing & ACL model — grant/revoke, role inheritance, permission resolution at query level

## Diagrams

- [ ] 7. Component diagram — services, databases, S3, client, and how they connect
- [ ] 8. Sequence diagrams — chunked upload, download, sync polling, share grant/revoke, auth
- [ ] 9. ER diagram — database entities and relationships
- [ ] 10. Class/struct diagram — Go packages, key structs, interfaces, dependencies

## Protocols & Flows

- [ ] 11. Upload/download flow — client → API → S3 presigned URL sequence, chunking, resume, hash verification
- [ ] 12. Sync protocol — cursor-based change feed, polling, conflict detection, "conflicted copy" resolution

## Project Setup

- [ ] 13. Project structure — Go module layout, server/cli split, shared packages
- [ ] 14. Local dev setup — docker-compose, env config, seed data, dev workflow
