package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/yashwant1810/miniDrive/internal/model"
)

// CreateUser inserts a new user and returns it.
func (s *PgStore) CreateUser(ctx context.Context, email, passwordHash string) (*model.User, error) {
	user := &model.User{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash)
		 VALUES ($1, $2)
		 RETURNING id, email, password_hash, quota_bytes, used_bytes, created_at`,
		email, passwordHash,
	).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.QuotaBytes, &user.UsedBytes, &user.CreatedAt)

	if err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}
	return user, nil
}

// GetUserByEmail looks up a user by email. Returns an error if not found.
func (s *PgStore) GetUserByEmail(ctx context.Context, email string) (*model.User, error) {
	user := &model.User{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, quota_bytes, used_bytes, created_at
		 FROM users WHERE email = $1`,
		email,
	).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.QuotaBytes, &user.UsedBytes, &user.CreatedAt)

	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}
	return user, nil
}

// GetUserByID looks up a user by ID. Returns an error if not found.
func (s *PgStore) GetUserByID(ctx context.Context, id uuid.UUID) (*model.User, error) {
	user := &model.User{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, quota_bytes, used_bytes, created_at
		 FROM users WHERE id = $1`,
		id,
	).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.QuotaBytes, &user.UsedBytes, &user.CreatedAt)

	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}
	return user, nil
}
