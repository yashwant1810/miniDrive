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

	// Health check
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})
}
