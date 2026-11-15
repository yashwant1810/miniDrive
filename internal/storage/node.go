package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/yashwant1810/miniDrive/internal/model"
)

// CreateNode inserts a new file or folder node.
func (s *PgStore) CreateNode(ctx context.Context, node *model.Node) (*model.Node, error) {
	result := &model.Node{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO nodes (owner_id, parent_id, name, type)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, owner_id, parent_id, name, type, current_version_id, deleted_at, created_at, updated_at`,
		node.OwnerID, node.ParentID, node.Name, node.Type,
	).Scan(
		&result.ID, &result.OwnerID, &result.ParentID, &result.Name, &result.Type,
		&result.CurrentVersionID, &result.DeletedAt, &result.CreatedAt, &result.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create node: %w", err)
	}
	return result, nil
}

// GetNode retrieves a single node by ID. Returns an error if not found or soft-deleted.
func (s *PgStore) GetNode(ctx context.Context, id uuid.UUID) (*model.Node, error) {
	node := &model.Node{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, owner_id, parent_id, name, type, current_version_id, deleted_at, created_at, updated_at
		 FROM nodes
		 WHERE id = $1 AND deleted_at IS NULL`,
		id,
	).Scan(
		&node.ID, &node.OwnerID, &node.ParentID, &node.Name, &node.Type,
		&node.CurrentVersionID, &node.DeletedAt, &node.CreatedAt, &node.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get node: %w", err)
	}
	return node, nil
}

// ListFolder returns the children of a folder, paginated by cursor (node name for alphabetical ordering).
// Pass parentID=nil for root-level items. ownerID scopes to the user's own files.
func (s *PgStore) ListFolder(ctx context.Context, parentID *uuid.UUID, ownerID uuid.UUID, cursor string, limit int) ([]model.Node, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	// Build query — parentID=nil means root (WHERE parent_id IS NULL)
	var rows pgx.Rows
	var err error

	if parentID == nil {
		rows, err = s.pool.Query(ctx,
			`SELECT id, owner_id, parent_id, name, type, current_version_id, deleted_at, created_at, updated_at
			 FROM nodes
			 WHERE parent_id IS NULL AND owner_id = $1 AND deleted_at IS NULL AND name > $2
			 ORDER BY name ASC
			 LIMIT $3`,
			ownerID, cursor, limit+1, // fetch one extra to determine if there's a next page
		)
	} else {
		rows, err = s.pool.Query(ctx,
			`SELECT id, owner_id, parent_id, name, type, current_version_id, deleted_at, created_at, updated_at
			 FROM nodes
			 WHERE parent_id = $1 AND deleted_at IS NULL AND name > $2
			 ORDER BY name ASC
			 LIMIT $3`,
			parentID, cursor, limit+1,
		)
	}

	if err != nil {
		return nil, "", fmt.Errorf("failed to list folder: %w", err)
	}
	defer rows.Close()

	var nodes []model.Node
	for rows.Next() {
		var n model.Node
		if err := rows.Scan(
			&n.ID, &n.OwnerID, &n.ParentID, &n.Name, &n.Type,
			&n.CurrentVersionID, &n.DeletedAt, &n.CreatedAt, &n.UpdatedAt,
		); err != nil {
			return nil, "", fmt.Errorf("failed to scan node: %w", err)
		}
		nodes = append(nodes, n)
	}

	// If we got limit+1 results, there's a next page. The cursor is the last item's name.
	var nextCursor string
	if len(nodes) > limit {
		nodes = nodes[:limit]                // trim the extra
		nextCursor = nodes[limit-1].Name     // cursor for next page
	}

	return nodes, nextCursor, nil
}

// UpdateNode renames and/or moves a node. Only non-nil fields are updated.
func (s *PgStore) UpdateNode(ctx context.Context, id uuid.UUID, name *string, parentID *uuid.UUID) (*model.Node, error) {
	// Build SET clause dynamically based on which fields are provided
	node := &model.Node{}
	now := time.Now()

	if name != nil && parentID != nil {
		// Rename + move
		err := s.pool.QueryRow(ctx,
			`UPDATE nodes SET name = $1, parent_id = $2, updated_at = $3
			 WHERE id = $4 AND deleted_at IS NULL
			 RETURNING id, owner_id, parent_id, name, type, current_version_id, deleted_at, created_at, updated_at`,
			*name, *parentID, now, id,
		).Scan(&node.ID, &node.OwnerID, &node.ParentID, &node.Name, &node.Type,
			&node.CurrentVersionID, &node.DeletedAt, &node.CreatedAt, &node.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to update node: %w", err)
		}
	} else if name != nil {
		// Rename only
		err := s.pool.QueryRow(ctx,
			`UPDATE nodes SET name = $1, updated_at = $2
			 WHERE id = $3 AND deleted_at IS NULL
			 RETURNING id, owner_id, parent_id, name, type, current_version_id, deleted_at, created_at, updated_at`,
			*name, now, id,
		).Scan(&node.ID, &node.OwnerID, &node.ParentID, &node.Name, &node.Type,
			&node.CurrentVersionID, &node.DeletedAt, &node.CreatedAt, &node.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to update node: %w", err)
		}
	} else if parentID != nil {
		// Move only
		err := s.pool.QueryRow(ctx,
			`UPDATE nodes SET parent_id = $1, updated_at = $2
			 WHERE id = $3 AND deleted_at IS NULL
			 RETURNING id, owner_id, parent_id, name, type, current_version_id, deleted_at, created_at, updated_at`,
			*parentID, now, id,
		).Scan(&node.ID, &node.OwnerID, &node.ParentID, &node.Name, &node.Type,
			&node.CurrentVersionID, &node.DeletedAt, &node.CreatedAt, &node.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to update node: %w", err)
		}
	} else {
		return nil, fmt.Errorf("nothing to update")
	}

	return node, nil
}

// SoftDeleteNode marks a node and all its descendants as deleted.
func (s *PgStore) SoftDeleteNode(ctx context.Context, id uuid.UUID) error {
	now := time.Now()

	// Use a recursive CTE to find all descendants, then soft-delete them all in one statement.
	_, err := s.pool.Exec(ctx,
		`WITH RECURSIVE descendants AS (
			SELECT id FROM nodes WHERE id = $1 AND deleted_at IS NULL
			UNION ALL
			SELECT n.id FROM nodes n
			JOIN descendants d ON n.parent_id = d.id
			WHERE n.deleted_at IS NULL
		)
		UPDATE nodes SET deleted_at = $2
		WHERE id IN (SELECT id FROM descendants)`,
		id, now,
	)
	if err != nil {
		return fmt.Errorf("failed to soft delete node: %w", err)
	}
	return nil
}
