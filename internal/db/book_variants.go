package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/vavallee/bindery/internal/models"
)

// BookVariantRepo stores the requested languages of each book and format.
type BookVariantRepo struct {
	db *sql.DB
}

func NewBookVariantRepo(db *sql.DB) *BookVariantRepo {
	return &BookVariantRepo{db: db}
}

// EnsureWanted creates missing variants without changing the status of rows
// already present. The returned rows contain only the requested languages and
// are ordered by canonical language code.
func (r *BookVariantRepo) EnsureWanted(ctx context.Context, bookID int64, mediaType string, languages []string) ([]models.BookVariant, error) {
	if mediaType != models.MediaTypeEbook && mediaType != models.MediaTypeAudiobook {
		return nil, fmt.Errorf("invalid variant media type %q", mediaType)
	}
	canonical, err := models.NormalizeRequestLanguages(languages)
	if err != nil {
		return nil, fmt.Errorf("normalize request languages: %w", err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin variants: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	variants := make([]models.BookVariant, 0, len(canonical))
	for _, language := range canonical {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO book_variants (book_id, media_type, language) VALUES (?, ?, ?)`, bookID, mediaType, language); err != nil {
			return nil, fmt.Errorf("ensure book variant: %w", err)
		}
		var variant models.BookVariant
		if err := tx.QueryRowContext(ctx, `SELECT id, book_id, media_type, language, status FROM book_variants
			WHERE book_id = ? AND media_type = ? AND language = ?`, bookID, mediaType, language).
			Scan(&variant.ID, &variant.BookID, &variant.MediaType, &variant.Language, &variant.Status); err != nil {
			return nil, fmt.Errorf("read book variant: %w", err)
		}
		variants = append(variants, variant)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit variants: %w", err)
	}
	return variants, nil
}

func (r *BookVariantRepo) ListByBook(ctx context.Context, bookID int64) ([]models.BookVariant, error) {
	return r.list(ctx, `SELECT id, book_id, media_type, language, status FROM book_variants
		WHERE book_id = ? ORDER BY media_type, language, id`, bookID)
}

func (r *BookVariantRepo) ListWanted(ctx context.Context) ([]models.BookVariant, error) {
	return r.list(ctx, `SELECT id, book_id, media_type, language, status FROM book_variants
		WHERE status = 'wanted' ORDER BY book_id, media_type, language, id`)
}

func (r *BookVariantRepo) list(ctx context.Context, query string, args ...any) ([]models.BookVariant, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list book variants: %w", err)
	}
	defer rows.Close()
	var variants []models.BookVariant
	for rows.Next() {
		var variant models.BookVariant
		if err := rows.Scan(&variant.ID, &variant.BookID, &variant.MediaType, &variant.Language, &variant.Status); err != nil {
			return nil, fmt.Errorf("scan book variant: %w", err)
		}
		variants = append(variants, variant)
	}
	return variants, rows.Err()
}

func (r *BookVariantRepo) SetStatus(ctx context.Context, variantID int64, status string) error {
	switch status {
	case models.VariantWanted, models.VariantDownloading, models.VariantImported, models.VariantReview:
	default:
		return fmt.Errorf("invalid variant status %q", status)
	}
	result, err := r.db.ExecContext(ctx, `UPDATE book_variants SET status = ? WHERE id = ?`, status, variantID)
	if err != nil {
		return fmt.Errorf("set book variant status: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count updated book variants: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("book variant %d not found", variantID)
	}
	return nil
}
