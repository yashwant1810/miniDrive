package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/yashwant1810/miniDrive/internal/auth"
	"github.com/yashwant1810/miniDrive/internal/model"
)

// --- Request/Response types ---

type initUploadRequest struct {
	ParentID    *uuid.UUID `json:"parent_id"`
	Name        string     `json:"name"`
	Size        int64      `json:"size"`
	ChunkHashes []string   `json:"chunk_hashes"`
}

type chunkResponse struct {
	Index         int    `json:"index"`
	Hash          string `json:"hash"`
	PresignedURL  string `json:"presigned_url,omitempty"`
	AlreadyExists bool   `json:"already_exists"`
}

type commitRequest struct {
	UploadID    string   `json:"upload_id"`
	ChunkHashes []string `json:"chunk_hashes"`
}

// --- Handlers ---

// handleInitUpload creates (or resumes) a file upload.
//
// Resume: If the Idempotency-Key header matches an existing UPLOADING version,
// the server returns the same file_id/upload_id and checks S3 for partially
// uploaded chunks, returning presigned URLs only for the missing ones.
//
// Dedup: Chunks already in the DB (from other files) are marked already_exists
// and get no presigned URL.
//
// Quota: If used_bytes + file size > quota_bytes the request is rejected 413.
func (h *Handler) handleInitUpload(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())

	var req initUploadRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}

	// --- Validate ---
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "name is required")
		return
	}
	if req.Size <= 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "size must be positive")
		return
	}
	if len(req.ChunkHashes) == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "chunk_hashes is required")
		return
	}
	for _, ch := range req.ChunkHashes {
		if len(ch) != 64 {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid chunk hash format (expected 64-char hex)")
			return
		}
	}

	// --- Quota check ---
	user, err := h.Store.GetUserByID(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to load user")
		return
	}
	if user.UsedBytes+req.Size > user.QuotaBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "QUOTA_EXCEEDED",
			fmt.Sprintf("quota exceeded: %d used + %d requested > %d limit",
				user.UsedBytes, req.Size, user.QuotaBytes))
		return
	}

	// --- Resume check (Idempotency-Key header) ---
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	var fileID uuid.UUID
	var uploadID uuid.UUID
	var resumed bool

	if idempotencyKey != "" {
		existingVersion, err := h.Store.GetUploadingVersionByKey(r.Context(), userID, idempotencyKey)
		if err == nil {
			// Found an existing upload — resume it
			fileID = existingVersion.NodeID
			uploadID = existingVersion.ID
			resumed = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to check idempotency key")
			return
		}
		// pgx.ErrNoRows → no existing upload, fall through to create new
	}

	if !resumed {
		// --- Verify parent ---
		if req.ParentID != nil {
			parent, err := h.Store.GetNode(r.Context(), *req.ParentID)
			if err != nil {
				writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "parent folder not found")
				return
			}
			if parent.Type != "folder" {
				writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "parent must be a folder")
				return
			}
			if parent.OwnerID != userID {
				writeError(w, http.StatusForbidden, "FORBIDDEN", "no access to parent folder")
				return
			}
		}

		// --- Create the file node ---
		node, err := h.Store.CreateNode(r.Context(), &model.Node{
			OwnerID:  userID,
			ParentID: req.ParentID,
			Name:     req.Name,
			Type:     "file",
		})
		if err != nil {
			if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique") {
				writeError(w, http.StatusConflict, "CONFLICT", "file with this name already exists here")
				return
			}
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create file")
			return
		}
		fileID = node.ID

		// --- Create version in UPLOADING status ---
		version, err := h.Store.CreateVersion(r.Context(), node.ID, req.Size, userID, idempotencyKey)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create version")
			return
		}
		uploadID = version.ID
	}

	// --- Build chunk responses ---
	// Step 1: Check DB for chunks that already exist (dedup across files)
	existingInDB, err := h.Store.GetExistingChunkHashes(r.Context(), req.ChunkHashes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to check existing chunks")
		return
	}

	// Step 2: For resumed uploads, also check S3 for chunks uploaded in a
	// previous attempt that weren't yet committed (not in DB, but in S3).
	existingInS3 := make(map[string]bool)
	if resumed {
		var needS3Check []string
		for _, hash := range req.ChunkHashes {
			if !existingInDB[hash] {
				needS3Check = append(needS3Check, hash)
			}
		}
		if len(needS3Check) > 0 {
			existingInS3 = h.checkChunksInS3(r, needS3Check)
		}
	}

	// Step 3: Generate presigned URLs only for chunks missing from both DB and S3
	chunks := make([]chunkResponse, len(req.ChunkHashes))
	for i, hash := range req.ChunkHashes {
		alreadyExists := existingInDB[hash] || existingInS3[hash]
		cr := chunkResponse{
			Index:         i,
			Hash:          hash,
			AlreadyExists: alreadyExists,
		}

		if !alreadyExists {
			s3Key := chunkS3Key(hash)
			url, err := h.S3.GeneratePresignedUploadURL(r.Context(), s3Key, 15*time.Minute)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to generate upload url")
				return
			}
			cr.PresignedURL = url
		}

		chunks[i] = cr
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"file_id":   fileID,
		"upload_id": uploadID,
		"chunks":    chunks,
	})
}

// checkChunksInS3 does parallel HEAD requests to find which chunks are already
// present in object storage (for resume — uploaded but not yet committed).
func (h *Handler) checkChunksInS3(r *http.Request, hashes []string) map[string]bool {
	type result struct {
		hash   string
		exists bool
	}

	results := make(chan result, len(hashes))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10) // concurrency limit

	for _, hash := range hashes {
		wg.Add(1)
		go func(chunkHash string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			s3Key := chunkS3Key(chunkHash)
			_, err := h.S3.HeadChunk(r.Context(), s3Key)
			results <- result{hash: chunkHash, exists: err == nil}
		}(hash)
	}

	wg.Wait()
	close(results)

	existing := make(map[string]bool)
	for res := range results {
		if res.exists {
			existing[res.hash] = true
		}
	}
	return existing
}

// handleCommit finalizes an upload after all chunks are in S3.
func (h *Handler) handleCommit(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())

	fileID, err := uuid.Parse(r.PathValue("file_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid file_id")
		return
	}

	var req commitRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}

	versionID, err := uuid.Parse(req.UploadID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid upload_id")
		return
	}

	// Verify the node exists and belongs to the user
	node, err := h.Store.GetNode(r.Context(), fileID)
	if err != nil {
		writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "file not found")
		return
	}
	if node.OwnerID != userID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "no access to this file")
		return
	}

	// Get the version
	version, err := h.Store.GetVersion(r.Context(), versionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "version not found")
		return
	}
	if version.NodeID != fileID {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "upload_id does not match file_id")
		return
	}

	// Already committed — idempotent
	if version.Status == "committed" {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"file_id":    fileID,
			"version_id": versionID,
			"name":       node.Name,
			"size":       version.Size,
			"created_at": version.CreatedAt,
		})
		return
	}

	// Validate chunk hashes from the request
	if len(req.ChunkHashes) == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "chunk_hashes is required")
		return
	}

	// Verify each chunk exists in S3 — parallel HEAD requests
	type chunkResult struct {
		index int
		chunk model.Chunk
		err   error
	}

	chunkResults := make([]chunkResult, len(req.ChunkHashes))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)

	for i, hash := range req.ChunkHashes {
		wg.Add(1)
		go func(idx int, chunkHash string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			s3Key := chunkS3Key(chunkHash)
			size, err := h.S3.HeadChunk(r.Context(), s3Key)
			chunkResults[idx] = chunkResult{
				index: idx,
				chunk: model.Chunk{Hash: chunkHash, Size: int(size), S3Key: s3Key},
				err:   err,
			}
		}(i, hash)
	}
	wg.Wait()

	var missingChunks []int
	var chunkModels []model.Chunk
	for _, res := range chunkResults {
		if res.err != nil {
			missingChunks = append(missingChunks, res.index)
		} else {
			chunkModels = append(chunkModels, res.chunk)
		}
	}

	if len(missingChunks) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "CHUNKS_MISSING",
				"message": "some chunks are missing from storage",
				"missing": missingChunks,
			},
		})
		return
	}

	// Upsert chunks (handles dedup ref_count)
	if err := h.Store.UpsertChunks(r.Context(), chunkModels); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to store chunk metadata")
		return
	}

	// Create version_chunks mapping
	versionChunks := make([]model.VersionChunk, len(req.ChunkHashes))
	for i, hash := range req.ChunkHashes {
		versionChunks[i] = model.VersionChunk{
			VersionID:  versionID.String(),
			ChunkIndex: i,
			ChunkHash:  hash,
		}
	}
	if err := h.Store.CreateVersionChunks(r.Context(), versionID, versionChunks); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to register chunks")
		return
	}

	// Commit the version (also increments user's used_bytes in the same tx)
	if err := h.Store.CommitVersion(r.Context(), versionID, fileID); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to commit version")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"file_id":    fileID,
		"version_id": versionID,
		"name":       node.Name,
		"size":       version.Size,
		"created_at": version.CreatedAt,
	})
}

// handleDownload returns presigned download URLs for all chunks of a file.
func (h *Handler) handleDownload(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())

	fileID, err := uuid.Parse(r.PathValue("file_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid file_id")
		return
	}

	// Get the file node
	node, err := h.Store.GetNode(r.Context(), fileID)
	if err != nil {
		writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "file not found")
		return
	}
	if node.OwnerID != userID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "no access to this file")
		return
	}
	if node.Type != "file" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "not a file")
		return
	}
	if node.CurrentVersionID == nil {
		writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "file has no committed version")
		return
	}

	// Optional: download a specific version
	versionID := *node.CurrentVersionID
	if vIDStr := r.URL.Query().Get("version_id"); vIDStr != "" {
		vID, err := uuid.Parse(vIDStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid version_id")
			return
		}
		versionID = vID
	}

	// Get the version
	version, err := h.Store.GetVersion(r.Context(), versionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "version not found")
		return
	}

	// Get ordered chunk list
	versionChunks, err := h.Store.GetVersionChunks(r.Context(), versionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to get chunks")
		return
	}

	// Generate presigned download URLs
	type downloadChunk struct {
		Index        int    `json:"index"`
		Hash         string `json:"hash"`
		PresignedURL string `json:"presigned_url"`
	}
	chunks := make([]downloadChunk, len(versionChunks))
	for i, vc := range versionChunks {
		s3Key := chunkS3Key(vc.ChunkHash)
		url, err := h.S3.GeneratePresignedDownloadURL(r.Context(), s3Key, 5*time.Minute)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to generate download url")
			return
		}
		chunks[i] = downloadChunk{
			Index:        vc.ChunkIndex,
			Hash:         vc.ChunkHash,
			PresignedURL: url,
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"file_id":    fileID,
		"version_id": versionID,
		"name":       node.Name,
		"size":       version.Size,
		"chunks":     chunks,
	})
}

// chunkS3Key builds the S3 object key from a chunk hash.
// Uses prefix distribution: ab/cd/abcdef1234... for even S3 partition load.
func chunkS3Key(hash string) string {
	return fmt.Sprintf("%s/%s/%s", hash[:2], hash[2:4], hash)
}
