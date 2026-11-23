package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/yashwant1810/miniDrive/internal/model"
)

// CreateVersion inserts a new version in UPLOADING status.
// If idempotencyKey is non-empty it is stored for resumable-upload lookup.
func (s *PgStore) CreateVersion(ctx context.Context, nodeID uuid.UUID, size int64, createdBy uuid.UUID, idempotencyKey string) (*model.Version, error) {
	v := &model.Version{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO versions (node_id, size, status, created_by, idempotency_key)
		 VALUES ($1, $2, 'uploading', $3, NULLIF($4, ''))
		 RETURNING id, node_id, size, status, created_by, idempotency_key, created_at`,
		nodeID, size, createdBy, idempotencyKey,
	).Scan(&v.ID, &v.NodeID, &v.Size, &v.Status, &v.CreatedBy, &v.IdempotencyKey, &v.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to create version: %w", err)
	}
	return v, nil
}

// GetUploadingVersionByKey finds an existing UPLOADING version by idempotency key + user.
// Returns pgx.ErrNoRows when nothing matches.
func (s *PgStore) GetUploadingVersionByKey(ctx context.Context, createdBy uuid.UUID, key string) (*model.Version, error) {
	v := &model.Version{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, node_id, size, status, created_by, idempotency_key, created_at
		 FROM versions
		 WHERE created_by = $1 AND idempotency_key = $2 AND status = 'uploading'`,
		createdBy, key,
	).Scan(&v.ID, &v.NodeID, &v.Size, &v.Status, &v.CreatedBy, &v.IdempotencyKey, &v.CreatedAt)
	if err != nil {
		return nil, err // caller checks pgx.ErrNoRows
	}
	return v, nil
}

// CommitVersion transitions a version from UPLOADING to COMMITTED,
// updates the node's current_version_id, and increments the user's used_bytes.
func (s *PgStore) CommitVersion(ctx context.Context, versionID uuid.UUID, nodeID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Update version status and get size + user for quota
	var size int64
	var createdBy uuid.UUID
	err = tx.QueryRow(ctx,
		`UPDATE versions SET status = 'committed'
		 WHERE id = $1 AND status = 'uploading'
		 RETURNING size, created_by`,
		versionID,
	).Scan(&size, &createdBy)
	if err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("version not found or already committed")
		}
		return fmt.Errorf("failed to commit version: %w", err)
	}

	// Point the node to this version
	_, err = tx.Exec(ctx,
		`UPDATE nodes SET current_version_id = $1, updated_at = now() WHERE id = $2`,
		versionID, nodeID,
	)
	if err != nil {
		return fmt.Errorf("failed to update node version: %w", err)
	}

	// Increment the user's used_bytes (quota tracking)
	_, err = tx.Exec(ctx,
		`UPDATE users SET used_bytes = used_bytes + $1 WHERE id = $2`,
		size, createdBy,
	)
	if err != nil {
		return fmt.Errorf("failed to update user quota: %w", err)
	}

	return tx.Commit(ctx)
}

// GetVersion retrieves a version by ID.
func (s *PgStore) GetVersion(ctx context.Context, id uuid.UUID) (*model.Version, error) {
	v := &model.Version{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, node_id, size, status, created_by, idempotency_key, created_at
		 FROM versions WHERE id = $1`,
		id,
	).Scan(&v.ID, &v.NodeID, &v.Size, &v.Status, &v.CreatedBy, &v.IdempotencyKey, &v.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to get version: %w", err)
	}
	return v, nil
}
