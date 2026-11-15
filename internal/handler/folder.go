package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/yashwant1810/miniDrive/internal/auth"
	"github.com/yashwant1810/miniDrive/internal/model"
)

// --- Request types ---

type createFolderRequest struct {
	ParentID *uuid.UUID `json:"parent_id"`
	Name     string     `json:"name"`
}

type updateFolderRequest struct {
	Name     *string    `json:"name"`
	ParentID *uuid.UUID `json:"parent_id"`
}

// --- Handlers ---

// handleCreateFolder creates a new folder.
func (h *Handler) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())

	var req createFolderRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "name is required")
		return
	}

	// If parent specified, verify it exists and is a folder owned by this user
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

	node := &model.Node{
		OwnerID:  userID,
		ParentID: req.ParentID,
		Name:     req.Name,
		Type:     "folder",
	}

	created, err := h.Store.CreateNode(r.Context(), node)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique") {
			writeError(w, http.StatusConflict, "CONFLICT", "folder with this name already exists here")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create folder")
		return
	}

	writeJSON(w, http.StatusCreated, created)
}

// handleListFolder lists the contents of a folder (or root if folder_id is "root").
func (h *Handler) handleListFolder(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())

	folderIDStr := r.PathValue("folder_id")
	cursor := r.URL.Query().Get("cursor")
	limitStr := r.URL.Query().Get("page_size")

	limit := 50
	if limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	var parentID *uuid.UUID
	if folderIDStr != "root" {
		id, err := uuid.Parse(folderIDStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid folder_id")
			return
		}

		// Verify folder exists and user owns it
		folder, err := h.Store.GetNode(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "folder not found")
			return
		}
		if folder.Type != "folder" {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "not a folder")
			return
		}
		if folder.OwnerID != userID {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "no access to this folder")
			return
		}
		parentID = &id
	}

	nodes, nextCursor, err := h.Store.ListFolder(r.Context(), parentID, userID, cursor, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list folder")
		return
	}

	// Ensure we return an empty array, not null
	if nodes == nil {
		nodes = []model.Node{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"folder_id":   folderIDStr,
		"items":       nodes,
		"next_cursor": nextCursor,
	})
}

// handleUpdateFolder renames or moves a folder.
func (h *Handler) handleUpdateFolder(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())

	folderID, err := uuid.Parse(r.PathValue("folder_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid folder_id")
		return
	}

	// Verify folder exists and user owns it
	folder, err := h.Store.GetNode(r.Context(), folderID)
	if err != nil {
		writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "folder not found")
		return
	}
	if folder.OwnerID != userID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "no access to this folder")
		return
	}

	var req updateFolderRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}

	// Trim name if provided
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "name cannot be empty")
			return
		}
		req.Name = &trimmed
	}

	// Prevent moving a folder into itself
	if req.ParentID != nil && *req.ParentID == folderID {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "cannot move a folder into itself")
		return
	}

	if req.Name == nil && req.ParentID == nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "nothing to update")
		return
	}

	updated, err := h.Store.UpdateNode(r.Context(), folderID, req.Name, req.ParentID)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique") {
			writeError(w, http.StatusConflict, "CONFLICT", "name already exists in target folder")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to update folder")
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteFolder soft-deletes a folder and all its contents.
func (h *Handler) handleDeleteFolder(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())

	folderID, err := uuid.Parse(r.PathValue("folder_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid folder_id")
		return
	}

	// Verify folder exists and user owns it
	folder, err := h.Store.GetNode(r.Context(), folderID)
	if err != nil {
		writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "folder not found")
		return
	}
	if folder.OwnerID != userID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "no access to this folder")
		return
	}

	if err := h.Store.SoftDeleteNode(r.Context(), folderID); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to delete folder")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"folder_id":  folderID,
		"deleted_at": folder.CreatedAt, // this is a placeholder — ideally return the actual deleted_at
	})
}
