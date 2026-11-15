package storage

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/yashwant1810/miniDrive/internal/model"
)

// UpsertChunks inserts chunk records, incrementing ref_count if the chunk already exists.
func (s *PgStore) UpsertChunks(ctx context.Context, chunks []model.Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	// Build a bulk INSERT with ON CONFLICT to handle dedup
	var sb strings.Builder
	args := make([]interface{}, 0, len(chunks)*4)
	sb.WriteString(`INSERT INTO chunks (hash, size, s3_key, ref_count) VALUES `)

	for i, c := range chunks {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * 4
		sb.WriteString(fmt.Sprintf("($%d, $%d, $%d, 1)", base+1, base+2, base+3))
		args = append(args, c.Hash, c.Size, c.S3Key)
		// We only use 3 params per chunk, but ref_count is hardcoded to 1
		// Fix the placeholder count
	}

	// Rebuild properly with 3 args per chunk
	sb.Reset()
	args = args[:0]
	sb.WriteString(`INSERT INTO chunks (hash, size, s3_key, ref_count) VALUES `)
	for i, c := range chunks {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i*3 + 1
		sb.WriteString(fmt.Sprintf("($%d, $%d, $%d, 1)", base, base+1, base+2))
		args = append(args, c.Hash, c.Size, c.S3Key)
	}
	sb.WriteString(` ON CONFLICT (hash) DO UPDATE SET ref_count = chunks.ref_count + 1`)

	_, err := s.pool.Exec(ctx, sb.String(), args...)
	if err != nil {
		return fmt.Errorf("failed to upsert chunks: %w", err)
	}
	return nil
}

// GetExistingChunkHashes checks which hashes already exist in the chunks table.
// Returns a map of hash → true for existing chunks (used for dedup).
func (s *PgStore) GetExistingChunkHashes(ctx context.Context, hashes []string) (map[string]bool, error) {
	if len(hashes) == 0 {
		return map[string]bool{}, nil
	}

	// Build IN clause
	args := make([]interface{}, len(hashes))
	placeholders := make([]string, len(hashes))
	for i, h := range hashes {
		args[i] = h
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}

	query := fmt.Sprintf(
		`SELECT hash FROM chunks WHERE hash IN (%s)`,
		strings.Join(placeholders, ", "),
	)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to check existing chunks: %w", err)
	}
	defer rows.Close()

	existing := make(map[string]bool)
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("failed to scan chunk hash: %w", err)
		}
		existing[hash] = true
	}
	return existing, nil
}

// CreateVersionChunks inserts the chunk-to-version mapping rows.
func (s *PgStore) CreateVersionChunks(ctx context.Context, versionID uuid.UUID, chunks []model.VersionChunk) error {
	if len(chunks) == 0 {
		return nil
	}

	var sb strings.Builder
	args := make([]interface{}, 0, len(chunks)*3+1)
	args = append(args, versionID)

	sb.WriteString(`INSERT INTO version_chunks (version_id, chunk_index, chunk_hash) VALUES `)
	for i, c := range chunks {
		if i > 0 {
			sb.WriteString(", ")
		}
		// $1 is always versionID, $N and $N+1 are index and hash
		base := i*2 + 2
		sb.WriteString(fmt.Sprintf("($1, $%d, $%d)", base, base+1))
		args = append(args, c.ChunkIndex, c.ChunkHash)
	}

	_, err := s.pool.Exec(ctx, sb.String(), args...)
	if err != nil {
		return fmt.Errorf("failed to create version chunks: %w", err)
	}
	return nil
}

// GetVersionChunks returns all chunks for a version, ordered by index.
func (s *PgStore) GetVersionChunks(ctx context.Context, versionID uuid.UUID) ([]model.VersionChunk, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT vc.version_id, vc.chunk_index, vc.chunk_hash
		 FROM version_chunks vc
		 WHERE vc.version_id = $1
		 ORDER BY vc.chunk_index`,
		versionID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get version chunks: %w", err)
	}
	defer rows.Close()

	var chunks []model.VersionChunk
	for rows.Next() {
		var c model.VersionChunk
		if err := rows.Scan(&c.VersionID, &c.ChunkIndex, &c.ChunkHash); err != nil {
			return nil, fmt.Errorf("failed to scan version chunk: %w", err)
		}
		chunks = append(chunks, c)
	}
	return chunks, nil
}
