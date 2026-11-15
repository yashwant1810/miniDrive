package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/yashwant1810/miniDrive/internal/model"
)

// CreateVersion inserts a new version in UPLOADING status.
func (s *PgStore) CreateVersion(ctx context.Context, nodeID uuid.UUID, size int64, createdBy uuid.UUID) (*model.Version, error) {
	v := &model.Version{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO versions (node_id, size, status, created_by)
		 VALUES ($1, $2, 'uploading', $3)
		 RETURNING id, node_id, size, status, created_by, created_at`,
		nodeID, size, createdBy,
	).Scan(&v.ID, &v.NodeID, &v.Size, &v.Status, &v.CreatedBy, &v.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to create version: %w", err)
	}
	return v, nil
}

// CommitVersion transitions a version from UPLOADING to COMMITTED
// and updates the node's current_version_id.
func (s *PgStore) CommitVersion(ctx context.Context, versionID uuid.UUID, nodeID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Update version status
	tag, err := tx.Exec(ctx,
		`UPDATE versions SET status = 'committed' WHERE id = $1 AND status = 'uploading'`,
		versionID,
	)
	if err != nil {
		return fmt.Errorf("failed to commit version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("version not found or already committed")
	}

	// Point the node to this version
	_, err = tx.Exec(ctx,
		`UPDATE nodes SET current_version_id = $1, updated_at = now() WHERE id = $2`,
		versionID, nodeID,
	)
	if err != nil {
		return fmt.Errorf("failed to update node version: %w", err)
	}

	return tx.Commit(ctx)
}

// GetVersion retrieves a version by ID.
func (s *PgStore) GetVersion(ctx context.Context, id uuid.UUID) (*model.Version, error) {
	v := &model.Version{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, node_id, size, status, created_by, created_at
		 FROM versions WHERE id = $1`,
		id,
	).Scan(&v.ID, &v.NodeID, &v.Size, &v.Status, &v.CreatedBy, &v.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to get version: %w", err)
	}
	return v, nil
}
