package model

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type ChangeLogEntry struct {
	Seq       int64           `json:"seq"`
	UserID    uuid.UUID       `json:"user_id"`
	NodeID    uuid.UUID       `json:"node_id"`
	Op        string          `json:"op"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}
