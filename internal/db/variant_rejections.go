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

// VariantRejectionRepo stores source identities scoped to the book they must
// never fill again. The caller must establish the source path's provenance;
// this repo enforces relational ownership and keeps the first verdict.
type VariantRejectionRepo struct {
	db *sql.DB
}

func NewVariantRejectionRepo(database *sql.DB) *VariantRejectionRepo {
	return &VariantRejectionRepo{db: database}
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
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (book_id, source_path) DO NOTHING`, bookID, variantID, downloadID, sourcePath, strings.TrimSpace(reason))
	if err != nil {
		return fmt.Errorf("record variant rejection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit variant rejection: %w", err)
	}
	return nil
}

// IsRejected must be handled fail-closed: a DB error is not a clean negative.
func (r *VariantRejectionRepo) IsRejected(ctx context.Context, bookID int64, sourcePath string) (bool, error) {
	if bookID <= 0 || !filepath.IsAbs(sourcePath) || filepath.Clean(sourcePath) != sourcePath {
		return false, fmt.Errorf("look up variant rejection: invalid book or source path")
	}
	var rejected bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM variant_rejections WHERE book_id = ? AND source_path = ?)`, bookID, sourcePath).Scan(&rejected); err != nil {
		return false, fmt.Errorf("look up variant rejection: %w", err)
	}
	return rejected, nil
}
