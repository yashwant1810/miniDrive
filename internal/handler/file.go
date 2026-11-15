package handler

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
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
	Index        int    `json:"index"`
	Hash         string `json:"hash"`
	PresignedURL string `json:"presigned_url,omitempty"`
	AlreadyExists bool  `json:"already_exists"`
}

type commitRequest struct {
	UploadID    string   `json:"upload_id"`
	ChunkHashes []string `json:"chunk_hashes"`
}

// --- Handlers ---

// handleInitUpload creates a file node + version, checks dedup, and returns presigned URLs.
func (h *Handler) handleInitUpload(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())

	var req initUploadRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}

	// Validate
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
	for _, h := range req.ChunkHashes {
		if len(h) != 64 {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid chunk hash format (expected 64-char hex)")
			return
		}
	}

	// Verify parent exists if specified
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

	// Check which chunks already exist (dedup)
	existing, err := h.Store.GetExistingChunkHashes(r.Context(), req.ChunkHashes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to check existing chunks")
		return
	}

	// Create the file node
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

	// Create version in UPLOADING status
	version, err := h.Store.CreateVersion(r.Context(), node.ID, req.Size, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create version")
		return
	}

	// Build chunk responses with presigned URLs for missing chunks
	chunks := make([]chunkResponse, len(req.ChunkHashes))
	for i, hash := range req.ChunkHashes {
		s3Key := chunkS3Key(hash)
		cr := chunkResponse{
			Index:         i,
			Hash:          hash,
			AlreadyExists: existing[hash],
		}

		if !existing[hash] {
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
		"file_id":   node.ID,
		"upload_id": version.ID,
		"chunks":    chunks,
	})
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

	// Verify each chunk exists in S3
	var missingChunks []int
	var chunkModels []model.Chunk
	for i, hash := range req.ChunkHashes {
		s3Key := chunkS3Key(hash)
		size, err := h.S3.HeadChunk(r.Context(), s3Key)
		if err != nil {
			missingChunks = append(missingChunks, i)
			continue
		}
		chunkModels = append(chunkModels, model.Chunk{
			Hash:  hash,
			Size:  int(size),
			S3Key: s3Key,
		})
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

	// Commit the version
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
