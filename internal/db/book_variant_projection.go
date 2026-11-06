package db

import (
	"context"
	"fmt"
	"strings"
)

// BookVariantStatus is read-only status/evidence, never an on-disk file or a
// download. It may be copied into a requester response without a path leak.
type BookVariantStatus struct {
	ID           int64
	MediaType    string
	Language     string
	Status       string
	Verification string
}

type LegacyBookFileStatus struct {
	MediaType string
}

type BookVariantProjection struct {
	Variants    []BookVariantStatus
	LegacyFiles []LegacyBookFileStatus
}

// ListStatusByBooks batches only the book IDs the caller already authorized
// and paginated. Do not pass client-supplied IDs without an ownership check.
// A file without a variant stays a separate unverified legacy entry: the
// catalogue language says nothing about that file's actual contents.
func (r *BookVariantRepo) ListStatusByBooks(ctx context.Context, bookIDs []int64) (map[int64]BookVariantProjection, error) {
	out := make(map[int64]BookVariantProjection, len(bookIDs))
	if len(bookIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(bookIDs))
	for i, id := range bookIDs {
		args[i] = id
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(bookIDs)), ",")
	// #nosec G202 -- the IN list contains only placeholders, never caller text.
	rows, err := r.db.QueryContext(ctx, `SELECT v.book_id, v.id, v.media_type, v.language, v.status,
		COALESCE(MAX(CASE WHEN f.verification = 'verified' AND f.language = v.language THEN 2
			WHEN f.verification = 'unverified' THEN 1 ELSE 0 END), 0)
		FROM book_variants v LEFT JOIN book_files f ON f.variant_id = v.id
			AND f.book_id = v.book_id AND f.format = v.media_type
		WHERE v.book_id IN (`+marks+`)
		GROUP BY v.id ORDER BY v.book_id, v.media_type, v.language, v.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("load book variant statuses: %w", err)
	}
	for rows.Next() {
		var id int64
		var v BookVariantStatus
		var evidence int
		if err := rows.Scan(&id, &v.ID, &v.MediaType, &v.Language, &v.Status, &evidence); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan book variant statuses: %w", err)
		}
		switch evidence {
		case 2:
			v.Verification = "verified"
		case 1:
			v.Verification = "unverified"
		}
		p := out[id]
		p.Variants = append(p.Variants, v)
		out[id] = p
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, fmt.Errorf("read book variant statuses: %w", err)
	}
	// #nosec G202 -- the IN list contains only placeholders, never caller text.
	rows, err = r.db.QueryContext(ctx, `SELECT f.book_id, f.format FROM book_files f
		WHERE f.variant_id IS NULL AND f.book_id IN (`+marks+`)
		ORDER BY f.book_id, f.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("load legacy book file statuses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var f LegacyBookFileStatus
		if err := rows.Scan(&id, &f.MediaType); err != nil {
			return nil, fmt.Errorf("scan legacy book file statuses: %w", err)
		}
		p := out[id]
		p.LegacyFiles = append(p.LegacyFiles, f)
		out[id] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read legacy book file statuses: %w", err)
	}
	return out, nil
}
