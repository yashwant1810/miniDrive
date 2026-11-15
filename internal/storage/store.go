package storage

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yashwant1810/miniDrive/internal/model"
)

// Store defines all database operations. Interfaces allow mocking in tests.
type Store interface {
	// Users
	CreateUser(ctx context.Context, email, passwordHash string) (*model.User, error)
	GetUserByEmail(ctx context.Context, email string) (*model.User, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (*model.User, error)

	// Refresh tokens
	CreateRefreshToken(ctx context.Context, userID uuid.UUID, tokenHash string) (*model.RefreshToken, error)
	ValidateRefreshToken(ctx context.Context, tokenHash string) (*model.RefreshToken, error)
	RevokeRefreshToken(ctx context.Context, tokenHash string, userID uuid.UUID) error
	RevokeAllRefreshTokens(ctx context.Context, userID uuid.UUID) error

	// Nodes (files + folders)
	CreateNode(ctx context.Context, node *model.Node) (*model.Node, error)
	GetNode(ctx context.Context, id uuid.UUID) (*model.Node, error)
	ListFolder(ctx context.Context, parentID *uuid.UUID, ownerID uuid.UUID, cursor string, limit int) ([]model.Node, string, error)
	UpdateNode(ctx context.Context, id uuid.UUID, name *string, parentID *uuid.UUID) (*model.Node, error)
	SoftDeleteNode(ctx context.Context, id uuid.UUID) error
}

// PgStore implements Store using a pgx connection pool.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore creates a new PgStore.
func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}
