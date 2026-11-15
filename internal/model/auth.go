package model

import (
	"time"

	"github.com/google/uuid"
)

type Claims struct {
	Sub   uuid.UUID `json:"sub"`
	Email string    `json:"email"`
	Exp   int64     `json:"exp"`
	Iat   int64     `json:"iat"`
	JTI   string    `json:"jti"`
}

type RefreshToken struct {
	ID        uuid.UUID `json:"id"`
	UserID    uuid.UUID `json:"user_id"`
	TokenHash string    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}
