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

// BookStatusAccess scopes a book to its current owner. OwnerUserID 0 is an
// explicitly unscoped library/admin read; a request uses its target owner,
// even when the caller is an admin acting with forUser.
type BookStatusAccess struct {
	BookID      int64
	OwnerUserID int64
}

func bookAccessValues(books []BookStatusAccess) (string, []any) {
	args := make([]any, 0, len(books)*2)
	for _, book := range books {
		args = append(args, book.BookID, book.OwnerUserID)
	}
	return strings.TrimSuffix(strings.Repeat("(?,?),", len(books)), ","), args
}

// ListStatusByBooks rechecks current ownership when it reads variant and
// legacy-file statuses, rather than trusting IDs authorized by an earlier
// page query. Unowned legacy books remain visible to scoped readers. A file
// without a variant stays a separate unverified entry: the catalogue
// language says nothing about that file's actual contents.
func (r *BookVariantRepo) ListStatusByBooks(ctx context.Context, books []BookStatusAccess) (map[BookStatusAccess]BookVariantProjection, error) {
	out := make(map[BookStatusAccess]BookVariantProjection, len(books))
	if len(books) == 0 {
		return out, nil
	}
	unique := make([]BookStatusAccess, 0, len(books))
	seen := make(map[BookStatusAccess]bool, len(books))
	for _, book := range books {
		if !seen[book] {
			seen[book] = true
			unique = append(unique, book)
		}
	}
	marks, args := bookAccessValues(unique)
	// #nosec G202 -- VALUES contains only placeholders, never caller text.
	rows, err := r.db.QueryContext(ctx, `WITH scope(book_id, owner_user_id) AS (VALUES `+marks+`)
		SELECT scope.book_id, scope.owner_user_id, v.id, v.media_type, v.language, v.status,
		COALESCE(MAX(CASE WHEN f.verification = 'verified' AND f.language = v.language THEN 2
			WHEN f.verification = 'unverified' THEN 1 ELSE 0 END), 0)
		FROM scope JOIN books b ON b.id = scope.book_id
			AND (scope.owner_user_id = 0 OR b.owner_user_id = scope.owner_user_id OR b.owner_user_id IS NULL)
		JOIN book_variants v ON v.book_id = scope.book_id
		LEFT JOIN book_files f ON f.variant_id = v.id
			AND f.book_id = v.book_id AND f.format = v.media_type
		GROUP BY scope.book_id, scope.owner_user_id, v.id
		ORDER BY scope.book_id, scope.owner_user_id, v.media_type, v.language, v.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("load book variant statuses: %w", err)
	}
	for rows.Next() {
		var book BookStatusAccess
		var v BookVariantStatus
		var evidence int
		if err := rows.Scan(&book.BookID, &book.OwnerUserID, &v.ID, &v.MediaType, &v.Language, &v.Status, &evidence); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan book variant statuses: %w", err)
		}
		switch evidence {
		case 2:
			v.Verification = "verified"
		case 1:
			v.Verification = "unverified"
		}
		p := out[book]
		p.Variants = append(p.Variants, v)
		out[book] = p
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, fmt.Errorf("read book variant statuses: %w", err)
	}
	// #nosec G202 -- VALUES contains only placeholders, never caller text.
	rows, err = r.db.QueryContext(ctx, `WITH scope(book_id, owner_user_id) AS (VALUES `+marks+`)
		SELECT scope.book_id, scope.owner_user_id, f.format FROM scope
		JOIN books b ON b.id = scope.book_id
			AND (scope.owner_user_id = 0 OR b.owner_user_id = scope.owner_user_id OR b.owner_user_id IS NULL)
		JOIN book_files f ON f.book_id = scope.book_id AND f.variant_id IS NULL
		ORDER BY scope.book_id, scope.owner_user_id, f.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("load legacy book file statuses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var book BookStatusAccess
		var f LegacyBookFileStatus
		if err := rows.Scan(&book.BookID, &book.OwnerUserID, &f.MediaType); err != nil {
			return nil, fmt.Errorf("scan legacy book file statuses: %w", err)
		}
		p := out[book]
		p.LegacyFiles = append(p.LegacyFiles, f)
		out[book] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read legacy book file statuses: %w", err)
	}
	return out, nil
}
