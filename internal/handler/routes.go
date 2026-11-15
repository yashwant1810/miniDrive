package handler

import (
	"net/http"

	"github.com/yashwant1810/miniDrive/internal/auth"
)

// RegisterRoutes maps all URL paths to their handlers.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Auth middleware — wraps all protected routes
	protected := auth.Middleware(h.Config.JWTSecret)

	// Public routes (no auth required)
	mux.HandleFunc("POST /v1/auth/signup", h.handleSignup)
	mux.HandleFunc("POST /v1/auth/login", h.handleLogin)
	mux.HandleFunc("POST /v1/auth/refresh", h.handleRefresh)

	// Protected routes (auth required)
	mux.Handle("POST /v1/auth/logout", protected(http.HandlerFunc(h.handleLogout)))

	// Folders
	mux.Handle("POST /v1/folders", protected(http.HandlerFunc(h.handleCreateFolder)))
	mux.Handle("GET /v1/folders/{folder_id}", protected(http.HandlerFunc(h.handleListFolder)))
	mux.Handle("PATCH /v1/folders/{folder_id}", protected(http.HandlerFunc(h.handleUpdateFolder)))
	mux.Handle("DELETE /v1/folders/{folder_id}", protected(http.HandlerFunc(h.handleDeleteFolder)))

	// Files
	mux.Handle("POST /v1/files", protected(http.HandlerFunc(h.handleInitUpload)))
	mux.Handle("POST /v1/files/{file_id}/commit", protected(http.HandlerFunc(h.handleCommit)))
	mux.Handle("GET /v1/files/{file_id}/download", protected(http.HandlerFunc(h.handleDownload)))

	// Health check
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})
}
