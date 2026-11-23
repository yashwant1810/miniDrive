#!/usr/bin/env bash
# Phase 2 Task 17: Quota enforcement e2e test
# Set a user's quota low → upload until quota hit → verify 413 response
# Also verify used_bytes increments on commit
set -euo pipefail

BASE="http://localhost:8081/v1"
DB_URL="postgres://minidrive:minidrive@localhost:5434/minidrive?sslmode=disable"
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

pass() { echo "✅ $1"; }
fail() { echo "❌ $1"; exit 1; }

# --- Sign up ---
echo "=== Setting up test user with tiny quota ==="
TS=$(date +%s%N)
EMAIL="quota-${TS}@test.com"
curl -s -X POST "$BASE/auth/signup" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$EMAIL\",\"password\":\"TestPass123!\"}" >/dev/null
TOKEN=$(curl -s -X POST "$BASE/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$EMAIL\",\"password\":\"TestPass123!\"}" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])")

# Set quota to 3 MB (3145728 bytes) — enough for 1 file but not 2
USER_ID=$(psql "$DB_URL" -tAc "SELECT id FROM users WHERE email = '$EMAIL'")
psql "$DB_URL" -c "UPDATE users SET quota_bytes = 3145728 WHERE id = '$USER_ID'" >/dev/null
pass "User quota set to 3 MB"

# Verify initial used_bytes = 0
USED_BEFORE=$(psql "$DB_URL" -tAc "SELECT used_bytes FROM users WHERE id = '$USER_ID'")
[[ "$USED_BEFORE" == "0" ]] || fail "Initial used_bytes should be 0, got $USED_BEFORE"
pass "Initial used_bytes = 0"

# --- Upload file 1 (2 MB) — should succeed ---
echo ""
echo "=== Upload #1 (2 MB) — should fit within 3 MB quota ==="
dd if=/dev/urandom of="$TMPDIR/chunk1" bs=1048576 count=2 2>/dev/null
HASH1=$(shasum -a 256 "$TMPDIR/chunk1" | awk '{print $1}')

INIT1=$(curl -s -X POST "$BASE/files" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d "{\"name\":\"small.bin\",\"size\":2097152,\"chunk_hashes\":[\"$HASH1\"]}")

FILE1_ID=$(echo "$INIT1" | python3 -c "import sys,json; print(json.load(sys.stdin)['file_id'])" 2>/dev/null) \
  || fail "Init #1 should succeed: $INIT1"
UPLOAD1_ID=$(echo "$INIT1" | python3 -c "import sys,json; print(json.load(sys.stdin)['upload_id'])")
pass "Init #1 accepted (2 MB < 3 MB quota)"

# Upload chunk and commit
URL1=$(echo "$INIT1" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][0]['presigned_url'])")
curl -s -o /dev/null -X PUT -T "$TMPDIR/chunk1" "$URL1"
COMMIT1=$(curl -s -X POST "$BASE/files/$FILE1_ID/commit" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d "{\"upload_id\":\"$UPLOAD1_ID\",\"chunk_hashes\":[\"$HASH1\"]}")
echo "$COMMIT1" | python3 -c "import sys,json; json.load(sys.stdin)['version_id']" >/dev/null 2>&1 \
  || fail "Commit #1 failed: $COMMIT1"
pass "File #1 committed"

# Verify used_bytes was incremented
USED_AFTER1=$(psql "$DB_URL" -tAc "SELECT used_bytes FROM users WHERE id = '$USER_ID'")
[[ "$USED_AFTER1" == "2097152" ]] || fail "used_bytes should be 2097152 after commit, got $USED_AFTER1"
pass "used_bytes = 2097152 (2 MB) after commit"

# --- Upload file 2 (2 MB) — should be rejected (2+2 > 3) ---
echo ""
echo "=== Upload #2 (2 MB) — should EXCEED 3 MB quota ==="
dd if=/dev/urandom of="$TMPDIR/chunk2" bs=1048576 count=2 2>/dev/null
HASH2=$(shasum -a 256 "$TMPDIR/chunk2" | awk '{print $1}')

INIT2_STATUS=$(curl -s -o "$TMPDIR/init2.json" -w "%{http_code}" -X POST "$BASE/files" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d "{\"name\":\"too-big.bin\",\"size\":2097152,\"chunk_hashes\":[\"$HASH2\"]}")

[[ "$INIT2_STATUS" == "413" ]] || fail "Expected HTTP 413, got $INIT2_STATUS: $(cat "$TMPDIR/init2.json")"
ERROR_CODE=$(cat "$TMPDIR/init2.json" | python3 -c "import sys,json; print(json.load(sys.stdin)['error']['code'])")
[[ "$ERROR_CODE" == "QUOTA_EXCEEDED" ]] || fail "Expected QUOTA_EXCEEDED error code, got $ERROR_CODE"
pass "Upload #2 rejected with 413 QUOTA_EXCEEDED (2 MB used + 2 MB requested > 3 MB limit)"

# --- Upload file 3 (1 MB) — should succeed (2+1 = 3 ≤ 3) ---
echo ""
echo "=== Upload #3 (1 MB) — should fit (2 MB used + 1 MB = 3 MB = quota) ==="
dd if=/dev/urandom of="$TMPDIR/chunk3" bs=1048576 count=1 2>/dev/null
HASH3=$(shasum -a 256 "$TMPDIR/chunk3" | awk '{print $1}')

INIT3=$(curl -s -X POST "$BASE/files" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d "{\"name\":\"just-fits.bin\",\"size\":1048576,\"chunk_hashes\":[\"$HASH3\"]}")

FILE3_ID=$(echo "$INIT3" | python3 -c "import sys,json; print(json.load(sys.stdin)['file_id'])" 2>/dev/null) \
  || fail "Init #3 should succeed: $INIT3"
UPLOAD3_ID=$(echo "$INIT3" | python3 -c "import sys,json; print(json.load(sys.stdin)['upload_id'])")
pass "Init #3 accepted (2 MB + 1 MB = 3 MB ≤ 3 MB quota)"

# Commit file 3
URL3=$(echo "$INIT3" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][0]['presigned_url'])")
curl -s -o /dev/null -X PUT -T "$TMPDIR/chunk3" "$URL3"
COMMIT3=$(curl -s -X POST "$BASE/files/$FILE3_ID/commit" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d "{\"upload_id\":\"$UPLOAD3_ID\",\"chunk_hashes\":[\"$HASH3\"]}")
echo "$COMMIT3" | python3 -c "import sys,json; json.load(sys.stdin)['version_id']" >/dev/null 2>&1 \
  || fail "Commit #3 failed: $COMMIT3"

USED_AFTER3=$(psql "$DB_URL" -tAc "SELECT used_bytes FROM users WHERE id = '$USER_ID'")
[[ "$USED_AFTER3" == "3145728" ]] || fail "used_bytes should be 3145728, got $USED_AFTER3"
pass "File #3 committed, used_bytes = 3145728 (quota fully used)"

# --- Upload file 4 (even 1 byte) — should be rejected ---
echo ""
echo "=== Upload #4 (1 MB) — quota fully used, should be rejected ==="
INIT4_STATUS=$(curl -s -o "$TMPDIR/init4.json" -w "%{http_code}" -X POST "$BASE/files" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d "{\"name\":\"overflow.bin\",\"size\":1048576,\"chunk_hashes\":[\"$HASH3\"]}")

[[ "$INIT4_STATUS" == "413" ]] || fail "Expected HTTP 413, got $INIT4_STATUS: $(cat "$TMPDIR/init4.json")"
pass "Upload #4 rejected — no room left"

echo ""
echo "🎉 TASK 17 PASSED: Quota enforced — 413 when exceeded, used_bytes tracked on commit!"
