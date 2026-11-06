package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

var ErrVariantRejectionAssociation = errors.New("book, variant and download do not belong together")

// VariantRejectionRepo stores source identities for a book's requested variant.
// The caller must establish the source path's provenance; this repo enforces
// relational ownership and keeps the first verdict per variant.
type VariantRejectionRepo struct {
	db *sql.DB
}

func NewVariantRejectionRepo(database *sql.DB) *VariantRejectionRepo {
	return &VariantRejectionRepo{db: database}
}

// RejectionRepo shares the scanner's book database, including in the API and
// tests. Rejection checks cannot silently be disabled by missing wiring.
func (r *BookRepo) RejectionRepo() *VariantRejectionRepo {
	return NewVariantRejectionRepo(r.db)
}

func (r *VariantRejectionRepo) Record(ctx context.Context, bookID, variantID, downloadID int64, sourcePath, reason string) error {
	if bookID <= 0 || variantID <= 0 || downloadID <= 0 || !filepath.IsAbs(sourcePath) || filepath.Clean(sourcePath) != sourcePath || strings.TrimSpace(reason) == "" {
		return fmt.Errorf("record variant rejection: invalid identifiers, source path or reason")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin variant rejection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var valid bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM book_variants v JOIN downloads d ON d.book_id = v.book_id
		WHERE v.id = ? AND v.book_id = ? AND d.id = ?
		AND (d.variant_id IS NULL OR d.variant_id = v.id))`, variantID, bookID, downloadID).Scan(&valid)
	if err != nil {
		return fmt.Errorf("check rejection ownership: %w", err)
	}
	if !valid {
		return ErrVariantRejectionAssociation
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO variant_rejections (book_id, variant_id, download_id, source_path, reason)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (book_id, variant_id, source_path) DO NOTHING`, bookID, variantID, downloadID, sourcePath, strings.TrimSpace(reason))
	if err != nil {
		return fmt.Errorf("record variant rejection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit variant rejection: %w", err)
	}
	return nil
}

// ListSourcePaths supplies all rejected source identities for inode-based
// legacy scan reconciliation. An unassociated library file cannot identify a
// variant, so a rejection for any variant of its book must still block it.
// The caller must bound and validate any source-folder walk.
func (r *VariantRejectionRepo) ListSourcePaths(ctx context.Context, bookID int64) ([]string, error) {
	if bookID <= 0 {
		return nil, fmt.Errorf("list variant rejections: invalid book")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT source_path FROM variant_rejections WHERE book_id = ?`, bookID)
	if err != nil {
		return nil, fmt.Errorf("list variant rejections: %w", err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("read variant rejection: %w", err)
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list variant rejections: %w", err)
	}
	return paths, nil
}

// ListVariantSourcePaths supplies the current variant's source identities for
// importer folder containment checks. Other variants must be re-assessed.
func (r *VariantRejectionRepo) ListVariantSourcePaths(ctx context.Context, bookID, variantID int64) ([]string, error) {
	if bookID <= 0 || variantID <= 0 {
		return nil, fmt.Errorf("list variant rejections: invalid book or variant")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT source_path FROM variant_rejections WHERE book_id = ? AND variant_id = ?`, bookID, variantID)
	if err != nil {
		return nil, fmt.Errorf("list variant rejections: %w", err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("read variant rejection: %w", err)
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list variant rejections: %w", err)
	}
	return paths, nil
}

// IsRejected must be handled fail-closed: a DB error is not a clean negative.
func (r *VariantRejectionRepo) IsRejected(ctx context.Context, bookID, variantID int64, sourcePath string) (bool, error) {
	if bookID <= 0 || variantID <= 0 || !filepath.IsAbs(sourcePath) || filepath.Clean(sourcePath) != sourcePath {
		return false, fmt.Errorf("look up variant rejection: invalid book, variant or source path")
	}
	var rejected bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM variant_rejections WHERE book_id = ? AND variant_id = ? AND source_path = ?)`, bookID, variantID, sourcePath).Scan(&rejected); err != nil {
		return false, fmt.Errorf("look up variant rejection: %w", err)
	}
	return rejected, nil
}
