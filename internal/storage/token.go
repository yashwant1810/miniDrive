package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/yashwant1810/miniDrive/internal/model"
)

// Refresh tokens expire after 7 days.
const refreshTokenExpiry = 7 * 24 * time.Hour

// CreateRefreshToken stores a hashed refresh token in the database.
func (s *PgStore) CreateRefreshToken(ctx context.Context, userID uuid.UUID, tokenHash string) (*model.RefreshToken, error) {
	rt := &model.RefreshToken{}
	expiresAt := time.Now().Add(refreshTokenExpiry)

	err := s.pool.QueryRow(ctx,
		`INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
		 VALUES ($1, $2, $3)
		 RETURNING id, user_id, token_hash, expires_at, created_at`,
		userID, tokenHash, expiresAt,
	).Scan(&rt.ID, &rt.UserID, &rt.TokenHash, &rt.ExpiresAt, &rt.CreatedAt)

	if err != nil {
		return nil, fmt.Errorf("failed to create refresh token: %w", err)
	}
	return rt, nil
}

// ValidateRefreshToken looks up a token by its hash.
// Returns an error if not found or expired.
func (s *PgStore) ValidateRefreshToken(ctx context.Context, tokenHash string) (*model.RefreshToken, error) {
	rt := &model.RefreshToken{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, token_hash, expires_at, created_at
		 FROM refresh_tokens
		 WHERE token_hash = $1 AND expires_at > now()`,
		tokenHash,
	).Scan(&rt.ID, &rt.UserID, &rt.TokenHash, &rt.ExpiresAt, &rt.CreatedAt)

	if err != nil {
		return nil, fmt.Errorf("invalid or expired refresh token: %w", err)
	}
	return rt, nil
}

// RevokeRefreshToken deletes a specific token. Used for logout.
func (s *PgStore) RevokeRefreshToken(ctx context.Context, tokenHash string, userID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM refresh_tokens WHERE token_hash = $1 AND user_id = $2`,
		tokenHash, userID,
	)
	if err != nil {
		return fmt.Errorf("failed to revoke refresh token: %w", err)
	}
	return nil
}

// RevokeAllRefreshTokens deletes all tokens for a user. Used on password change.
func (s *PgStore) RevokeAllRefreshTokens(ctx context.Context, userID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM refresh_tokens WHERE user_id = $1`,
		userID,
	)
	if err != nil {
		return fmt.Errorf("failed to revoke all refresh tokens: %w", err)
	}
	return nil
}
