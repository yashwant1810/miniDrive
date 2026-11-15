package model

import (
	"time"

	"github.com/google/uuid"
)

type Node struct {
	ID               uuid.UUID  `json:"id"`
	OwnerID          uuid.UUID  `json:"owner_id"`
	ParentID         *uuid.UUID `json:"parent_id"`
	Name             string     `json:"name"`
	Type             string     `json:"type"`
	CurrentVersionID *uuid.UUID `json:"current_version_id,omitempty"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}
