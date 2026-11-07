package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vavallee/bindery/internal/models"
)

// BookVariantRepo stores the requested languages of each book and format.
type BookVariantRepo struct {
	db *sql.DB
}

func NewBookVariantRepo(db *sql.DB) *BookVariantRepo {
	return &BookVariantRepo{db: db}
}

// VariantRepo shares a book repository's database, so every book-add path
// records its variants even when no separate repository is wired by its caller.
func (r *BookRepo) VariantRepo() *BookVariantRepo {
	return NewBookVariantRepo(r.db)
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

// ListByBookIDs reads variants for a bulk search in bounded batches. Missing
// map entries mean legacy books with no variants; an unreadable batch is an
// error, never a reason to fall back to legacy file-path eligibility.
func (r *BookVariantRepo) ListByBookIDs(ctx context.Context, bookIDs []int64) (map[int64][]models.BookVariant, error) {
	byBook := make(map[int64][]models.BookVariant)
	const batchSize = 900 // remain below SQLite's older 999-parameter limit
	for start := 0; start < len(bookIDs); start += batchSize {
		batch := bookIDs[start:min(start+batchSize, len(bookIDs))]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		variants, err := r.list(ctx, `SELECT id, book_id, media_type, language, status FROM book_variants
			WHERE book_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`)
			ORDER BY book_id, media_type, language, id`, args...)
		if err != nil {
			return nil, err
		}
		for _, variant := range variants {
			byBook[variant.BookID] = append(byBook[variant.BookID], variant)
		}
	}
	return byBook, nil
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

// reconcileVariantAfterDetach runs in the caller's ownership-change transaction.
// An associated file satisfies the variant even when its language evidence
// is unverified. An in-flight download keeps a fileless variant busy;
// imported downloads alone are history, not proof the file is still here.
// Review stays a manual decision rather than being reopened automatically.
func reconcileVariantAfterDetach(ctx context.Context, tx *sql.Tx, variantID sql.NullInt64) error {
	if !variantID.Valid {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE book_variants SET status = CASE
		WHEN EXISTS (SELECT 1 FROM book_files f
			WHERE f.variant_id = book_variants.id AND f.book_id = book_variants.book_id
			AND f.format = book_variants.media_type
			AND (f.verification = 'verified' AND f.language = book_variants.language
				OR f.verification = 'unverified')) THEN 'imported'
		WHEN EXISTS (SELECT 1 FROM downloads d
			WHERE d.variant_id = book_variants.id AND d.book_id = book_variants.book_id
			AND d.status IN (?, ?, ?, ?, ?, ?, ?, ?)) THEN 'downloading'
		ELSE 'wanted' END
		WHERE id = ? AND status IN ('imported', 'downloading')`,
		models.StateGrabbed, models.StateDownloading, models.StateCompleted,
		models.StateImportPending, models.StateImporting, models.StateImportFailed,
		models.StateImportExternal, models.StateImportHeld, variantID.Int64)
	if err != nil {
		return fmt.Errorf("reconcile detached book variant %d: %w", variantID.Int64, err)
	}
	return nil
}

// RetryReviewed moves only a reviewed, fileless, idle copy back to wanted.
// The status change and named audit are one transaction; failed or duplicate
// retries leave both untouched. Exhausted importFailed rows for this copy
// become importBlocked in that transaction so a later client poll cannot undo
// the new-release decision. Torrent identities, errors, rejections and files
// stay intact; this never contacts a client or starts a search.
func (r *BookVariantRepo) RetryReviewed(ctx context.Context, bookID, variantID, actorID int64, reason string) (bool, error) {
	reason = strings.TrimSpace(reason)
	if bookID <= 0 || variantID <= 0 || actorID <= 0 || reason == "" || len(reason) > 500 {
		return false, fmt.Errorf("review retry requires book, variant, actor and reason (max 500 bytes)")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin reviewed variant retry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE book_variants SET status = 'wanted'
		WHERE id = ? AND book_id = ? AND status = 'review'
		AND NOT EXISTS (SELECT 1 FROM book_files f WHERE f.variant_id = book_variants.id)
		AND NOT EXISTS (SELECT 1 FROM downloads d WHERE d.variant_id = book_variants.id AND `+variantActiveSQL+`)`,
		variantID, bookID)
	if err != nil {
		return false, fmt.Errorf("retry reviewed variant: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count reviewed variant retries: %w", err)
	}
	if n != 1 {
		return false, nil
	}
	// Recovery or the fifth failed attempt already put this copy in review,
	// but the scanner would still terminally block an exhausted importFailed
	// row on its next ordinary poll and reconcile wanted back to review.
	// Park only those spent rows; retryable failures are active and were
	// excluded by the claim above. The named old-torrent action can rearm an
	// importBlocked row if the copy is later reviewed again.
	if _, err := tx.ExecContext(ctx, `UPDATE downloads SET status=?, dead_at=?
		WHERE variant_id=? AND book_id=? AND status=? AND import_retry_count >= ?`,
		models.StateImportBlocked, time.Now().UTC(), variantID, bookID, models.StateImportFailed, ImportRetryLimit); err != nil {
		return false, fmt.Errorf("park exhausted reviewed downloads: %w", err)
	}
	data, err := json.Marshal(struct {
		ActorUserID int64  `json:"actorUserId"`
		VariantID   int64  `json:"variantId"`
		Reason      string `json:"reason"`
	}{actorID, variantID, reason})
	if err != nil {
		return false, fmt.Errorf("encode reviewed variant retry audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO history (book_id, event_type, source_title, data, created_at)
		VALUES (?, 'variantReviewRetried', '', ?, ?)`, bookID, string(data), time.Now().UTC()); err != nil {
		return false, fmt.Errorf("audit reviewed variant retry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit reviewed variant retry: %w", err)
	}
	return true, nil
}

// MarkDownloading records a successfully sent grab without overwriting an
// imported or review decision made while the download was being sent.
func (r *BookVariantRepo) MarkDownloading(ctx context.Context, variantID int64) (bool, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE book_variants SET status = 'downloading'
		WHERE id = ? AND status = 'wanted' AND EXISTS (
			SELECT 1 FROM downloads WHERE variant_id = ? AND `+variantActiveSQL+`)`, variantID, variantID)
	if err != nil {
		return false, fmt.Errorf("mark book variant downloading: %w", err)
	}
	count, err := result.RowsAffected()
	return count == 1, err
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
