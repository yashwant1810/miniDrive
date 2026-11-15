package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/yashwant1810/miniDrive/internal/auth"
)

// --- Request/Response types ---

type signupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type logoutRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// --- Handlers ---

// handleSignup creates a new user account.
func (h *Handler) handleSignup(w http.ResponseWriter, r *http.Request) {
	var req signupRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}

	// Validate
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" || !strings.Contains(req.Email, "@") {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid email")
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "password must be at least 8 characters")
		return
	}

	// Hash password
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to process password")
		return
	}

	// Create user
	user, err := h.Store.CreateUser(r.Context(), req.Email, hash)
	if err != nil {
		// pgx returns a unique violation for duplicate emails
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique") {
			writeError(w, http.StatusConflict, "CONFLICT", "email already registered")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create user")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"user_id": user.ID,
		"email":   user.Email,
	})
}

// handleLogin authenticates a user and returns access + refresh tokens.
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))

	// Look up user — same error message for "not found" and "wrong password" (no user enumeration)
	user, err := h.Store.GetUserByEmail(r.Context(), req.Email)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid email or password")
		return
	}

	// Check password
	if err := auth.CheckPassword(req.Password, user.PasswordHash); err != nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid email or password")
		return
	}

	// Generate access token (JWT)
	accessToken, err := auth.GenerateAccessToken(user.ID, user.Email, h.Config.JWTSecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to generate token")
		return
	}

	// Generate refresh token — random 32 bytes, store the SHA-256 hash in DB
	refreshTokenRaw, err := generateRandomToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to generate refresh token")
		return
	}
	refreshTokenHash := hashToken(refreshTokenRaw)

	_, err = h.Store.CreateRefreshToken(r.Context(), user.ID, refreshTokenHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to store refresh token")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"access_token":  accessToken,
		"refresh_token": refreshTokenRaw,
		"expires_in":    900, // 15 minutes in seconds
	})
}

// handleRefresh issues a new access token using a valid refresh token.
func (h *Handler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}

	if req.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "refresh_token is required")
		return
	}

	// Hash the raw token and look it up
	tokenHash := hashToken(req.RefreshToken)
	rt, err := h.Store.ValidateRefreshToken(r.Context(), tokenHash)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or expired refresh token")
		return
	}

	// Look up the user to get their email for the JWT claims
	user, err := h.Store.GetUserByID(r.Context(), rt.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to look up user")
		return
	}

	accessToken, err := auth.GenerateAccessToken(user.ID, user.Email, h.Config.JWTSecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to generate token")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"access_token": accessToken,
		"expires_in":   900,
	})
}

// handleLogout revokes a refresh token.
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	var req logoutRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return
	}

	if req.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "refresh_token is required")
		return
	}

	// Get user_id from the JWT in the Authorization header
	userID := auth.UserIDFromContext(r.Context())

	tokenHash := hashToken(req.RefreshToken)
	_ = h.Store.RevokeRefreshToken(r.Context(), tokenHash, userID)

	w.WriteHeader(http.StatusNoContent)
}

// --- Helper functions ---

// generateRandomToken generates a cryptographically random 32-byte hex token.
func generateRandomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashToken returns the SHA-256 hex digest of a token string.
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
