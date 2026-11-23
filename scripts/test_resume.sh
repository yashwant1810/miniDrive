#!/usr/bin/env bash
# Phase 2 Task 14: Resumable upload e2e test
# Upload 3-chunk file → simulate mid-upload failure (only 2 chunks in S3)
# → re-init with same Idempotency-Key → verify only 1 chunk needs upload
# → commit → verify file complete
set -euo pipefail

BASE="http://localhost:8081/v1"
DB_URL="postgres://minidrive:minidrive@localhost:5434/minidrive?sslmode=disable"
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

pass() { echo "✅ $1"; }
fail() { echo "❌ $1"; exit 1; }

# --- Auth helper ---
auth() {
  local TS=$(date +%s%N)
  local EMAIL="resume-${TS}@test.com"
  curl -s -X POST "$BASE/auth/signup" \
    -H "Content-Type: application/json" \
    -d "{\"email\":\"$EMAIL\",\"password\":\"TestPass123!\"}" >/dev/null
  curl -s -X POST "$BASE/auth/login" \
    -H "Content-Type: application/json" \
    -d "{\"email\":\"$EMAIL\",\"password\":\"TestPass123!\"}" \
    | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])"
}

# --- Generate 3 chunks ---
echo "=== Generating 3 test chunks ==="
dd if=/dev/urandom of="$TMPDIR/chunk0" bs=1048576 count=2 2>/dev/null
dd if=/dev/urandom of="$TMPDIR/chunk1" bs=1048576 count=2 2>/dev/null
dd if=/dev/urandom of="$TMPDIR/chunk2" bs=1048576 count=1 2>/dev/null
cat "$TMPDIR/chunk0" "$TMPDIR/chunk1" "$TMPDIR/chunk2" > "$TMPDIR/original.bin"

HASH0=$(shasum -a 256 "$TMPDIR/chunk0" | awk '{print $1}')
HASH1=$(shasum -a 256 "$TMPDIR/chunk1" | awk '{print $1}')
HASH2=$(shasum -a 256 "$TMPDIR/chunk2" | awk '{print $1}')
ORIG_SHA=$(shasum -a 256 "$TMPDIR/original.bin" | awk '{print $1}')
echo "Hashes: $HASH0, $HASH1, $HASH2"

TOKEN=$(auth)
IDEM_KEY="resume-test-$(date +%s%N)"
pass "Authenticated"

# --- Step 1: Init upload with Idempotency-Key ---
echo ""
echo "=== Step 1: Init upload (first attempt) ==="
INIT1=$(curl -s -X POST "$BASE/files" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: $IDEM_KEY" \
  -d "{
    \"name\": \"resume-test.bin\",
    \"size\": 5242880,
    \"chunk_hashes\": [\"$HASH0\", \"$HASH1\", \"$HASH2\"]
  }")

FILE_ID=$(echo "$INIT1" | python3 -c "import sys,json; print(json.load(sys.stdin)['file_id'])")
UPLOAD_ID=$(echo "$INIT1" | python3 -c "import sys,json; print(json.load(sys.stdin)['upload_id'])")
pass "Init: file_id=$FILE_ID, upload_id=$UPLOAD_ID"

# --- Step 2: Upload only chunks 0 and 1 (simulate failure before chunk 2) ---
echo ""
echo "=== Step 2: Upload chunks 0 and 1 (simulating failure before chunk 2) ==="
URL0=$(echo "$INIT1" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][0]['presigned_url'])")
URL1=$(echo "$INIT1" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][1]['presigned_url'])")
curl -s -o /dev/null -X PUT -T "$TMPDIR/chunk0" "$URL0"
curl -s -o /dev/null -X PUT -T "$TMPDIR/chunk1" "$URL1"
pass "Uploaded chunks 0 and 1 to S3 — chunk 2 NOT uploaded (simulated failure)"

# --- Step 3: Re-init with same Idempotency-Key (resume) ---
echo ""
echo "=== Step 3: Re-init with same Idempotency-Key (resume) ==="
INIT2=$(curl -s -X POST "$BASE/files" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: $IDEM_KEY" \
  -d "{
    \"name\": \"resume-test.bin\",
    \"size\": 5242880,
    \"chunk_hashes\": [\"$HASH0\", \"$HASH1\", \"$HASH2\"]
  }")

FILE_ID2=$(echo "$INIT2" | python3 -c "import sys,json; print(json.load(sys.stdin)['file_id'])")
UPLOAD_ID2=$(echo "$INIT2" | python3 -c "import sys,json; print(json.load(sys.stdin)['upload_id'])")

# Verify same IDs returned (resumed, not new)
[[ "$FILE_ID2" == "$FILE_ID" ]] || fail "file_id changed on resume: $FILE_ID → $FILE_ID2"
[[ "$UPLOAD_ID2" == "$UPLOAD_ID" ]] || fail "upload_id changed on resume: $UPLOAD_ID → $UPLOAD_ID2"
pass "Resume returned same file_id and upload_id"

# Chunks 0 and 1 should be already_exists (found in S3), chunk 2 should need upload
EXISTS0=$(echo "$INIT2" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][0]['already_exists'])")
EXISTS1=$(echo "$INIT2" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][1]['already_exists'])")
EXISTS2=$(echo "$INIT2" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][2]['already_exists'])")
[[ "$EXISTS0" == "True" ]] || fail "Chunk 0 should be already_exists on resume, got $EXISTS0"
[[ "$EXISTS1" == "True" ]] || fail "Chunk 1 should be already_exists on resume, got $EXISTS1"
[[ "$EXISTS2" == "False" ]] || fail "Chunk 2 should NOT be already_exists, got $EXISTS2"
pass "Resume correctly identified: chunks 0,1 in S3 — only chunk 2 needs upload"

# Verify: no URL for 0,1 — URL present for 2
HAS_URL0=$(echo "$INIT2" | python3 -c "import sys,json; c=json.load(sys.stdin)['chunks'][0]; print(c.get('presigned_url',''))")
HAS_URL1=$(echo "$INIT2" | python3 -c "import sys,json; c=json.load(sys.stdin)['chunks'][1]; print(c.get('presigned_url',''))")
URL2=$(echo "$INIT2" | python3 -c "import sys,json; c=json.load(sys.stdin)['chunks'][2]; print(c.get('presigned_url',''))")
[[ -z "$HAS_URL0" ]] || fail "Chunk 0 should NOT have a presigned URL on resume"
[[ -z "$HAS_URL1" ]] || fail "Chunk 1 should NOT have a presigned URL on resume"
[[ -n "$URL2" ]] || fail "Chunk 2 should have a presigned URL"
pass "Presigned URL returned only for missing chunk 2"

# --- Step 4: Upload the missing chunk ---
echo ""
echo "=== Step 4: Upload only chunk 2 ==="
curl -s -o /dev/null -X PUT -T "$TMPDIR/chunk2" "$URL2"
pass "Uploaded chunk 2 (1 MB)"

# --- Step 5: Commit ---
echo ""
echo "=== Step 5: Commit ==="
COMMIT=$(curl -s -X POST "$BASE/files/$FILE_ID/commit" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d "{\"upload_id\":\"$UPLOAD_ID\",\"chunk_hashes\":[\"$HASH0\",\"$HASH1\",\"$HASH2\"]}")
COMMIT_VID=$(echo "$COMMIT" | python3 -c "import sys,json; print(json.load(sys.stdin)['version_id'])" 2>/dev/null) \
  || fail "Commit failed: $COMMIT"
pass "Committed: version_id=$COMMIT_VID"

# --- Step 6: Download and verify integrity ---
echo ""
echo "=== Step 6: Download and verify integrity ==="
DL=$(curl -s "$BASE/files/$FILE_ID/download" -H "Authorization: Bearer $TOKEN")
DL_URL0=$(echo "$DL" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][0]['presigned_url'])")
DL_URL1=$(echo "$DL" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][1]['presigned_url'])")
DL_URL2=$(echo "$DL" | python3 -c "import sys,json; print(json.load(sys.stdin)['chunks'][2]['presigned_url'])")

curl -s -o "$TMPDIR/dl_0" "$DL_URL0"
curl -s -o "$TMPDIR/dl_1" "$DL_URL1"
curl -s -o "$TMPDIR/dl_2" "$DL_URL2"
cat "$TMPDIR/dl_0" "$TMPDIR/dl_1" "$TMPDIR/dl_2" > "$TMPDIR/reassembled.bin"

DL_SHA=$(shasum -a 256 "$TMPDIR/reassembled.bin" | awk '{print $1}')
[[ "$DL_SHA" == "$ORIG_SHA" ]] || fail "Integrity mismatch: $ORIG_SHA vs $DL_SHA"
pass "File integrity verified after resume (SHA-256: $DL_SHA)"

echo ""
echo "🎉 TASK 14 PASSED: Resumable upload — interrupted upload continued from where it left off!"
