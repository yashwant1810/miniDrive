package handler

import (
	"encoding/json"
	"net/http"

	"github.com/yashwant1810/miniDrive/internal/config"
	"github.com/yashwant1810/miniDrive/internal/s3client"
	"github.com/yashwant1810/miniDrive/internal/storage"
)

// Handler holds dependencies shared across all HTTP handlers.
type Handler struct {
	Store    storage.Store
	S3       s3client.S3Client
	Config   *config.Config
}

// New creates a new Handler.
func New(store storage.Store, s3 s3client.S3Client, cfg *config.Config) *Handler {
	return &Handler{
		Store:  store,
		S3:     s3,
		Config: cfg,
	}
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

// readJSON decodes a JSON request body into the target struct.
func readJSON(r *http.Request, target interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(target)
}
