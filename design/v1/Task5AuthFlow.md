# MiniDrive — Auth Flow (v1)

## Overview

MiniDrive uses email/password authentication with bcrypt hashing and a dual-token system:
- **Access token**: short-lived JWT (15 min), stateless, verified by signature only
- **Refresh token**: long-lived opaque token (7 days), stored server-side as a SHA-256 hash, revocable

The API never stores raw passwords or raw refresh tokens.

---

## 1. Signup

```
Client                          API                             PostgreSQL
  │                              │                                │
  │  POST /auth/signup           │                                │
  │  { email, password }         │                                │
  │─────────────────────────────►│                                │
  │                              │  Validate:                     │
  │                              │    - email format              │
  │                              │    - password ≥ 8 chars        │
  │                              │                                │
  │                              │  Check email not taken         │
  │                              │───────────────────────────────►│
  │                              │◄───────────────────────────────│
  │                              │                                │
  │                              │  hash = bcrypt(password, 12)   │
  │                              │                                │
  │                              │  INSERT INTO users             │
  │                              │  (email, password_hash)        │
  │                              │───────────────────────────────►│
  │                              │◄───────────────────────────────│
  │                              │                                │
  │  201 { user_id, email }      │                                │
  │◄─────────────────────────────│                                │
```

**Details:**
- bcrypt cost factor: **12** (hashes in ~250ms, good balance of security vs latency)
- Signup does NOT return tokens — the user must login separately. This keeps the flow simple and avoids auto-login assumptions.
- If email already exists: return `409 CONFLICT`

**Validation rules:**
- Email: standard RFC 5322 format check
- Password: minimum 8 characters. No complexity rules (they don't help, length matters more)

---

## 2. Login

```
Client                          API                             PostgreSQL
  │                              │                                │
  │  POST /auth/login            │                                │
  │  { email, password }         │                                │
  │─────────────────────────────►│                                │
  │                              │  SELECT id, password_hash      │
  │                              │  FROM users WHERE email = $1   │
  │                              │───────────────────────────────►│
  │                              │◄───────────────────────────────│
  │                              │                                │
  │                              │  bcrypt.Compare(password,      │
  │                              │                password_hash)  │
  │                              │                                │
  │                              │  If mismatch → 401             │
  │                              │                                │
  │                              │  Generate access_token (JWT)   │
  │                              │  Generate refresh_token        │
  │                              │    (random 32 bytes, hex)      │
  │                              │                                │
  │                              │  INSERT INTO refresh_tokens    │
  │                              │  (user_id, token_hash,         │
  │                              │   expires_at)                  │
  │                              │───────────────────────────────►│
  │                              │◄───────────────────────────────│
  │                              │                                │
  │  200 { access_token,         │                                │
  │        refresh_token,        │                                │
  │        expires_in: 900 }     │                                │
  │◄─────────────────────────────│                                │
```

**Details:**
- Same `401` response for "email not found" and "wrong password" — don't leak which one failed
- `refresh_token` stored as `SHA-256(token)` — raw token only ever held by client
- `expires_in: 900` = 15 minutes (access token lifetime in seconds)

---

## 3. JWT Structure

### Header

```json
{
  "alg": "HS256",
  "typ": "JWT"
}
```

Using HS256 (HMAC-SHA256) with a shared secret. Simpler than RS256 for a single-service architecture. At scale with multiple services, switch to RS256 (asymmetric) so services can verify without the signing key.

### Payload

```json
{
  "sub": "user-uuid",
  "email": "user@example.com",
  "iat": 1732100000,
  "exp": 1732100900,
  "jti": "unique-token-id"
}
```

| Claim | Type | Purpose |
|---|---|---|
| `sub` | UUID | User ID. Used by middleware to set request context. |
| `email` | string | Convenience. Avoids a DB lookup for display purposes. |
| `iat` | unix timestamp | Issued at. For auditing. |
| `exp` | unix timestamp | Expiry. `iat + 900` (15 min). Middleware rejects expired tokens. |
| `jti` | UUID | Unique token ID. Used for blacklisting if needed (see RealWorldQuestions.md). |

**What's NOT in the JWT:**
- Password hash (obviously)
- Role or permissions — these change and must be checked live from the DB
- Refresh token — separate concern, never in the JWT

### Signing

```go
token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
signed, err := token.SignedString([]byte(os.Getenv("JWT_SECRET")))
```

- `JWT_SECRET`: random 256-bit key, stored in env var, never in code
- Same key signs and verifies (HS256)

---

## 4. Token Refresh

```
Client                          API                             PostgreSQL
  │                              │                                │
  │  POST /auth/refresh          │                                │
  │  { refresh_token }           │                                │
  │─────────────────────────────►│                                │
  │                              │  hash = SHA-256(refresh_token) │
  │                              │                                │
  │                              │  SELECT user_id, expires_at    │
  │                              │  FROM refresh_tokens           │
  │                              │  WHERE token_hash = $1         │
  │                              │───────────────────────────────►│
  │                              │◄───────────────────────────────│
  │                              │                                │
  │                              │  If not found → 401            │
  │                              │  If expired → 401              │
  │                              │    + DELETE the row            │
  │                              │                                │
  │                              │  Generate new access_token     │
  │                              │  (same user_id, fresh exp)     │
  │                              │                                │
  │  200 { access_token,         │                                │
  │        expires_in: 900 }     │                                │
  │◄─────────────────────────────│                                │
```

**Details:**
- Refresh does NOT issue a new refresh token (no rotation at MVP). The same refresh token is reused until it expires or is revoked.
- At scale, add **refresh token rotation**: each refresh call issues a new refresh token and invalidates the old one. This limits the damage window if a refresh token is stolen.
- Expired refresh tokens are deleted on lookup — passive cleanup.

---

## 5. Auth Middleware

Every request (except `/auth/signup` and `/auth/login`) passes through the auth middleware.

```
Request
  │
  ▼
┌─────────────────────────────────────────┐
│  1. Extract Authorization header        │
│     Missing? → 401 UNAUTHORIZED         │
│     Not "Bearer <token>"? → 401         │
├─────────────────────────────────────────┤
│  2. Parse and verify JWT                │
│     Invalid signature? → 401            │
│     Malformed? → 401                    │
├─────────────────────────────────────────┤
│  3. Check expiry                        │
│     exp < now()? → 401                  │
├─────────────────────────────────────────┤
│  4. Extract claims                      │
│     sub (user_id), email, jti           │
├─────────────────────────────────────────┤
│  5. Set request context                 │
│     ctx = context.WithValue(ctx,        │
│       "user_id", claims.Sub)            │
├─────────────────────────────────────────┤
│  6. Pass to next handler                │
└─────────────────────────────────────────┘
  │
  ▼
Handler (has access to user_id from context)
```

**Go implementation sketch:**

```go
func AuthMiddleware(secret []byte) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            // 1. Extract token
            header := r.Header.Get("Authorization")
            if !strings.HasPrefix(header, "Bearer ") {
                http.Error(w, `{"error":{"code":"UNAUTHORIZED"}}`, 401)
                return
            }
            tokenStr := strings.TrimPrefix(header, "Bearer ")

            // 2-3. Parse, verify signature, check expiry
            claims := &Claims{}
            token, err := jwt.ParseWithClaims(tokenStr, claims,
                func(t *jwt.Token) (interface{}, error) {
                    return secret, nil
                })
            if err != nil || !token.Valid {
                http.Error(w, `{"error":{"code":"UNAUTHORIZED"}}`, 401)
                return
            }

            // 4-5. Set context
            ctx := context.WithValue(r.Context(), userIDKey, claims.Subject)
            
            // 6. Next handler
            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}
```

**Key points:**
- No DB call in the middleware. JWT verification is pure crypto — fast, stateless.
- The middleware does NOT check if the user still exists or if their account is disabled. That's a tradeoff: a deleted user's JWT works until it expires (max 15 min). Acceptable at MVP. For stricter control, add a token blacklist check (Redis).
- Error messages are identical for all auth failures — don't reveal why the token is invalid.

---

## 6. Middleware Chain

Requests pass through middleware in order. Each layer either passes the request along or short-circuits with an error.

```
Request
  │
  ▼
Rate Limiter  ──→  check per-user (or per-IP for auth endpoints) bucket
  │
  ▼
Request ID    ──→  generate UUID, set X-Request-Id header, add to context
  │
  ▼
Logger        ──→  log method, path, start time (log duration on response)
  │
  ▼
Auth          ──→  verify JWT, set user_id in context (skip for public routes)
  │
  ▼
Handler       ──→  business logic (handler checks ACLs per-resource)
```

**Public routes** (skip auth middleware):
- `POST /auth/signup`
- `POST /auth/login`

**All other routes**: auth middleware required.

**ACL checks are NOT in the middleware** — they're in the handler, because they're resource-specific. The middleware only answers "who is this user?" The handler answers "can this user do this to this resource?"

---

## 7. Logout

```
Client                          API                             PostgreSQL
  │                              │                                │
  │  POST /auth/logout           │                                │
  │  Authorization: Bearer <jwt> │                                │
  │  { refresh_token }           │                                │
  │─────────────────────────────►│                                │
  │                              │  hash = SHA-256(refresh_token) │
  │                              │                                │
  │                              │  DELETE FROM refresh_tokens    │
  │                              │  WHERE token_hash = $1         │
  │                              │  AND user_id = $2              │
  │                              │───────────────────────────────►│
  │                              │◄───────────────────────────────│
  │                              │                                │
  │  204 No Content              │                                │
  │◄─────────────────────────────│                                │
```

**Details:**
- Deletes the refresh token. The access JWT remains valid until expiry (max 15 min).
- The `AND user_id = $2` check ensures a user can only revoke their own refresh tokens.
- Logout is best-effort — even if the client loses the refresh token, it expires in 7 days.

---

## 8. Password Change

```
Client                          API                             PostgreSQL
  │                              │                                │
  │  POST /auth/password         │                                │
  │  Authorization: Bearer <jwt> │                                │
  │  { current_password,         │                                │
  │    new_password }            │                                │
  │─────────────────────────────►│                                │
  │                              │  Verify current_password       │
  │                              │  against stored hash           │
  │                              │───────────────────────────────►│
  │                              │◄───────────────────────────────│
  │                              │                                │
  │                              │  If mismatch → 401             │
  │                              │                                │
  │                              │  new_hash = bcrypt(new_pass)   │
  │                              │                                │
  │                              │  BEGIN TRANSACTION             │
  │                              │    UPDATE users                │
  │                              │    SET password_hash = $1      │
  │                              │                                │
  │                              │    DELETE FROM refresh_tokens  │
  │                              │    WHERE user_id = $2          │
  │                              │  COMMIT                        │
  │                              │───────────────────────────────►│
  │                              │◄───────────────────────────────│
  │                              │                                │
  │  200 { message: "updated" }  │                                │
  │◄─────────────────────────────│                                │
```

**Details:**
- Deletes ALL refresh tokens for the user in the same transaction as the password update. This forces re-login on all devices.
- The current session's access JWT still works until it expires (15 min). The client should immediately login again to get fresh tokens.
- `current_password` is required to prevent account takeover if a JWT is stolen.

---

## 9. Security Considerations

### Token Storage (Client Side)

| Client | Access Token | Refresh Token |
|---|---|---|
| CLI | In-memory or `~/.minidrive/credentials` (file permissions 0600) | Same file |
| Web UI | Memory only (JS variable). NOT localStorage. | httpOnly, secure, sameSite cookie |

- **CLI**: the credentials file is like `~/.docker/config.json` — standard pattern for CLI tools. Warn user to keep it private.
- **Web UI**: storing JWTs in localStorage is vulnerable to XSS. httpOnly cookies for the refresh token prevent JS access. Access token in memory is lost on page refresh — the client silently refreshes using the cookie.

### Brute Force Protection

- Rate limit `/auth/login` at **5 attempts per minute per IP**
- After 10 consecutive failures for an email, add a **progressive delay** (1s, 2s, 4s, 8s...) before responding. Don't lock the account — that's a DoS vector.
- Log all auth failures with IP and email for monitoring.

### JWT Secret Management

- Generate: `openssl rand -hex 32` → 256-bit key
- Store in env var `JWT_SECRET`, never in code or config files committed to git
- In production: AWS Secrets Manager or Parameter Store
- Rotation: deploy new secret, accept both old and new during a 15-min overlap (max JWT lifetime), then drop the old one

### What NOT To Do

- Don't store raw passwords (bcrypt only)
- Don't store raw refresh tokens (SHA-256 hash only)
- Don't put sensitive data in JWT payload (it's base64, not encrypted)
- Don't use JWT for sessions that need instant revocation (use refresh token revocation instead)
- Don't return different error messages for "user not found" vs "wrong password"

---

## 10. Token Lifecycle Summary

```
                    ┌─────────────────────────────────────────────┐
                    │              SIGNUP                          │
                    │  email + password → bcrypt → store user      │
                    └──────────────────────┬──────────────────────┘
                                           │
                                           ▼
                    ┌─────────────────────────────────────────────┐
                    │              LOGIN                           │
                    │  verify password → issue access + refresh    │
                    └───────┬─────────────────────────┬───────────┘
                            │                         │
                   access_token (JWT)          refresh_token
                   lives 15 min                lives 7 days
                   stateless                   stored in DB (hashed)
                            │                         │
                            ▼                         │
                    ┌───────────────┐                 │
                    │  API REQUESTS │                 │
                    │  (middleware)  │                 │
                    └───────┬───────┘                 │
                            │                         │
                     expires after 15m                │
                            │                         │
                            ▼                         ▼
                    ┌─────────────────────────────────────────────┐
                    │              REFRESH                         │
                    │  refresh_token → verify hash → new access    │
                    └─────────────────────────────────────────────┘
                            │
                     repeat until refresh expires or...
                            │
                            ▼
              ┌──────────────────────────────┐
              │  LOGOUT / PASSWORD CHANGE     │
              │  delete refresh token(s)      │
              │  access JWT expires naturally  │
              └──────────────────────────────┘
```
