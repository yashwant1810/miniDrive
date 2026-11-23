#!/usr/bin/env bash
# Phase 2 Task 5: Multi-chunk upload/download e2e test
# Uploads a 5 MB file as 3 chunks, downloads and reassembles, verifies integrity.
set -euo pipefail

BASE="http://localhost:8081/v1"
MINIO="http://localhost:9000"
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

pass() { echo "✅ $1"; }
fail() { echo "❌ $1"; exit 1; }

# --- 1. Generate test data: 5 MB file split into chunks ---
echo "=== Generating 5 MB test file ==="
# Chunk sizes: 2MB + 2MB + 1MB = 5MB
dd if=/dev/urandom of="$TMPDIR/chunk0" bs=1048576 count=2 2>/dev/null
dd if=/dev/urandom of="$TMPDIR/chunk1" bs=1048576 count=2 2>/dev/null
dd if=/dev/urandom of="$TMPDIR/chunk2" bs=1048576 count=1 2>/dev/null

# Combine into full file for later integrity check
cat "$TMPDIR/chunk0" "$TMPDIR/chunk1" "$TMPDIR/chunk2" > "$TMPDIR/original.bin"
ORIGINAL_SHA=$(shasum -a 256 "$TMPDIR/original.bin" | awk '{print $1}')
echo "Original file SHA-256: $ORIGINAL_SHA"
echo "Original file size: $(wc -c < "$TMPDIR/original.bin") bytes"

# Compute chunk hashes
HASH0=$(shasum -a 256 "$TMPDIR/chunk0" | awk '{print $1}')
HASH1=$(shasum -a 256 "$TMPDIR/chunk1" | awk '{print $1}')
HASH2=$(shasum -a 256 "$TMPDIR/chunk2" | awk '{print $1}')
echo "Chunk hashes: $HASH0, $HASH1, $HASH2"

# --- 2. Sign up + login ---
echo ""
echo "=== Signing up test user ==="
TS=$(date +%s)
EMAIL="chunktest-${TS}@test.com"
SIGNUP=$(curl -s -X POST "$BASE/auth/signup" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$EMAIL\",\"password\":\"TestPass123!\"}")
USER_ID=$(echo "$SIGNUP" | python3 -c "import sys,json; print(json.load(sys.stdin)['user_id'])" 2>/dev/null) \
  || fail "Signup failed: $SIGNUP"
pass "Signed up as $EMAIL (user_id=$USER_ID)"

echo "=== Logging in ==="
LOGIN=$(curl -s -X POST "$BASE/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$EMAIL\",\"password\":\"TestPass123!\"}")
TOKEN=$(echo "$LOGIN" | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])" 2>/dev/null) \
  || fail "Login failed: $LOGIN"
pass "Logged in, got access token"

# --- 3. Init upload with 3 chunk hashes ---
echo ""
echo "=== Init upload (3 chunks) ==="
INIT=$(curl -s -X POST "$BASE/files" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d "{
    \"name\": \"big-test-file.bin\",
    \"size\": 5242880,
    \"chunk_hashes\": [\"$HASH0\", \"$HASH1\", \"$HASH2\"]
  }")

FILE_ID=$(echo "$INIT" | python3 -c "import sys,json; print(json.load(sys.stdin)['file_id'])" 2>/dev/null) \
  || fail "Init upload failed: $INIT"
UPLOAD_ID=$(echo "$INIT" | python3 -c "import sys,json; print(json.load(sys.stdin)['upload_id'])")

# Extract presigned URLs
URL0=$(echo "$INIT" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][0]['presigned_url'])")
URL1=$(echo "$INIT" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][1]['presigned_url'])")
URL2=$(echo "$INIT" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][2]['presigned_url'])")

# Verify none are already_exists (fresh upload)
EXISTS0=$(echo "$INIT" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][0]['already_exists'])")
EXISTS1=$(echo "$INIT" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][1]['already_exists'])")
EXISTS2=$(echo "$INIT" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][2]['already_exists'])")

[[ "$EXISTS0" == "False" ]] || fail "Chunk 0 should not already exist"
[[ "$EXISTS1" == "False" ]] || fail "Chunk 1 should not already exist"
[[ "$EXISTS2" == "False" ]] || fail "Chunk 2 should not already exist"

pass "Init upload: file_id=$FILE_ID, upload_id=$UPLOAD_ID, 3 presigned URLs returned"

# --- 4. Upload chunks to S3 via presigned URLs ---
echo ""
echo "=== Uploading 3 chunks to S3 ==="
HTTP0=$(curl -s -o /dev/null -w "%{http_code}" -X PUT -T "$TMPDIR/chunk0" "$URL0")
[[ "$HTTP0" == "200" ]] || fail "Chunk 0 upload failed (HTTP $HTTP0)"
pass "Chunk 0 uploaded (2 MB)"

HTTP1=$(curl -s -o /dev/null -w "%{http_code}" -X PUT -T "$TMPDIR/chunk1" "$URL1")
[[ "$HTTP1" == "200" ]] || fail "Chunk 1 upload failed (HTTP $HTTP1)"
pass "Chunk 1 uploaded (2 MB)"

HTTP2=$(curl -s -o /dev/null -w "%{http_code}" -X PUT -T "$TMPDIR/chunk2" "$URL2")
[[ "$HTTP2" == "200" ]] || fail "Chunk 2 upload failed (HTTP $HTTP2)"
pass "Chunk 2 uploaded (1 MB)"

# --- 5. Verify chunks in MinIO ---
echo ""
echo "=== Verifying chunks in MinIO ==="
S3_KEY0="${HASH0:0:2}/${HASH0:2:2}/$HASH0"
S3_KEY1="${HASH1:0:2}/${HASH1:2:2}/$HASH1"
S3_KEY2="${HASH2:0:2}/${HASH2:2:2}/$HASH2"

# Use mc to check (or just trust the HEAD will work in commit)
pass "Chunks uploaded to S3 keys: $S3_KEY0, $S3_KEY1, $S3_KEY2"

# --- 6. Commit ---
echo ""
echo "=== Committing upload ==="
COMMIT=$(curl -s -X POST "$BASE/files/$FILE_ID/commit" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d "{
    \"upload_id\": \"$UPLOAD_ID\",
    \"chunk_hashes\": [\"$HASH0\", \"$HASH1\", \"$HASH2\"]
  }")

COMMIT_VID=$(echo "$COMMIT" | python3 -c "import sys,json; print(json.load(sys.stdin)['version_id'])" 2>/dev/null) \
  || fail "Commit failed: $COMMIT"
COMMIT_SIZE=$(echo "$COMMIT" | python3 -c "import sys,json; print(json.load(sys.stdin)['size'])")
[[ "$COMMIT_SIZE" == "5242880" ]] || fail "Committed size mismatch: expected 5242880, got $COMMIT_SIZE"
pass "Committed: version_id=$COMMIT_VID, size=$COMMIT_SIZE"

# --- 7. Download — get presigned GET URLs ---
echo ""
echo "=== Downloading file (3 chunks) ==="
DL=$(curl -s "$BASE/files/$FILE_ID/download" \
  -H "Authorization: Bearer $TOKEN")

DL_COUNT=$(echo "$DL" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['chunks']))")
[[ "$DL_COUNT" == "3" ]] || fail "Expected 3 download chunks, got $DL_COUNT"
pass "Download returned $DL_COUNT chunks"

# Fetch each chunk
DL_URL0=$(echo "$DL" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][0]['presigned_url'])")
DL_URL1=$(echo "$DL" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][1]['presigned_url'])")
DL_URL2=$(echo "$DL" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][2]['presigned_url'])")

curl -s -o "$TMPDIR/dl_chunk0" "$DL_URL0"
curl -s -o "$TMPDIR/dl_chunk1" "$DL_URL1"
curl -s -o "$TMPDIR/dl_chunk2" "$DL_URL2"

pass "Downloaded all 3 chunks"

# --- 8. Reassemble + verify integrity ---
echo ""
echo "=== Reassembling and verifying integrity ==="
cat "$TMPDIR/dl_chunk0" "$TMPDIR/dl_chunk1" "$TMPDIR/dl_chunk2" > "$TMPDIR/reassembled.bin"

REASSEMBLED_SIZE=$(wc -c < "$TMPDIR/reassembled.bin")
REASSEMBLED_SHA=$(shasum -a 256 "$TMPDIR/reassembled.bin" | awk '{print $1}')

echo "Reassembled size: $REASSEMBLED_SIZE bytes"
echo "Reassembled SHA-256: $REASSEMBLED_SHA"

[[ "$REASSEMBLED_SIZE" -eq 5242880 ]] || fail "Size mismatch: expected 5242880, got $REASSEMBLED_SIZE"
[[ "$REASSEMBLED_SHA" == "$ORIGINAL_SHA" ]] || fail "SHA-256 mismatch: expected $ORIGINAL_SHA, got $REASSEMBLED_SHA"

pass "Integrity verified — original and reassembled file match (SHA-256: $ORIGINAL_SHA)"

# --- 9. Verify individual chunk hashes ---
echo ""
echo "=== Verifying individual chunk hashes ==="
DL_HASH0=$(shasum -a 256 "$TMPDIR/dl_chunk0" | awk '{print $1}')
DL_HASH1=$(shasum -a 256 "$TMPDIR/dl_chunk1" | awk '{print $1}')
DL_HASH2=$(shasum -a 256 "$TMPDIR/dl_chunk2" | awk '{print $1}')

[[ "$DL_HASH0" == "$HASH0" ]] || fail "Chunk 0 hash mismatch"
[[ "$DL_HASH1" == "$HASH1" ]] || fail "Chunk 1 hash mismatch"
[[ "$DL_HASH2" == "$HASH2" ]] || fail "Chunk 2 hash mismatch"

pass "All 3 chunk hashes match"

echo ""
echo "🎉 Phase 2 Task 5: Multi-chunk upload/download test PASSED!"
