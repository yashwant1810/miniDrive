package model

import (
	"time"

	"github.com/google/uuid"
)

type Share struct {
	ID        uuid.UUID `json:"id"`
	NodeID    uuid.UUID `json:"node_id"`
	GranteeID uuid.UUID `json:"grantee_id"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}
