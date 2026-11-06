package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vavallee/bindery/internal/models"
)

// BookFileRepo manages the book_files table.
type BookFileRepo struct {
	db *sql.DB
}

// NewBookFileRepo creates a new BookFileRepo backed by the given database.
func NewBookFileRepo(db *sql.DB) *BookFileRepo {
	return &BookFileRepo{db: db}
}

// Fingerprint reads a cheap snapshot of book_files — its row count and its
// highest id — for a cache keyed on the table's actual contents rather than a
// counter this repo maintains itself (the manual-import scan's tracked-file
// index, #2480).
//
// An earlier version of that cache was keyed on an atomic counter bumped by
// this repo's own mutating methods, which went stale: book_files rows also
// disappear through the books(id) ON DELETE CASCADE FK when a book or author
// is deleted (BookRepo.Delete, AuthorRepo.Delete), and through
// BookRepo.UntrackFilePath's rollback DELETE — neither goes through this repo,
// so neither could bump its counter. Reading count+maxID directly from the
// table instead catches every mutation regardless of which code path made it:
// a row removed without a matching insert changes the count, and any insert
// hands out a strictly larger AUTOINCREMENT id, so the pair only repeats when
// nothing actually changed. The query is a single indexed aggregate, cheap
// enough to run on every scan request.
func (r *BookFileRepo) Fingerprint(ctx context.Context) (count int64, maxID int64, err error) {
	err = r.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(id), 0) FROM book_files`).Scan(&count, &maxID)
	if err != nil {
		return 0, 0, fmt.Errorf("book_files fingerprint: %w", err)
	}
	return count, maxID, nil
}

// pathEpochs holds one counter per database, bumped by every in place rewrite
// of book_files.path (UpdatePath). Fingerprint's (count, maxID) pair catches
// inserts and deletes however they happen, including the FK cascade, but an
// UPDATE changes neither number, so the manual-import scan's tracked-file
// cache (#2480) keys on this as well. It is keyed by *sql.DB rather than held
// on the repo because production builds more than one BookFileRepo over the
// same database (BookRepo owns one, the importer wiring another), and the
// rename has to be visible to every reader regardless of which one wrote it.
var pathEpochs sync.Map // *sql.DB -> *atomic.Uint64

func pathEpochFor(db *sql.DB) *atomic.Uint64 {
	v, _ := pathEpochs.LoadOrStore(db, new(atomic.Uint64))
	return v.(*atomic.Uint64)
}

// PathEpoch returns the in place path rewrite counter for this database. Read
// it before Fingerprint and the rows a cache is built from: a rename that
// lands mid rebuild then shows up as a changed epoch on the next read, so the
// cache errs toward rebuilding, never toward serving a stale path.
func (r *BookFileRepo) PathEpoch() uint64 {
	return pathEpochFor(r.db).Load()
}

// PathOwnedError reports that a path is already tracked in book_files by a
// different, existing book. book_files.path is globally UNIQUE, so the row
// cannot be recorded for a second book; before #2937 the insert was a silent
// OR IGNORE that left the caller believing it had tracked the file. The owner
// is named so the message tells the user where the file actually is.
type PathOwnedError struct {
	Path        string
	OwnerBookID int64
	OwnerTitle  string
}

func (e *PathOwnedError) Error() string {
	return fmt.Sprintf("%s is already tracked on another book, %q (id %d)", e.Path, e.OwnerTitle, e.OwnerBookID)
}

// TrackOutcome says what Track did with a path.
type TrackOutcome int

const (
	// TrackInserted means a new row was written for the book.
	TrackInserted TrackOutcome = iota + 1
	// TrackAlreadyTracked means the same book already held the path, so the
	// call was an idempotent no-op (retries and rescans rely on this).
	TrackAlreadyTracked
	// TrackReclaimedOrphan means the path was held by a row whose book no
	// longer exists (foreign_keys lost, #1727). That row is stale, so it was
	// re-pointed at the book.
	TrackReclaimedOrphan
)

// TrackResult is the outcome of Track. PreviousBookID is the dead book id an
// orphaned row named before it was reclaimed.
type TrackResult struct {
	Outcome        TrackOutcome
	PreviousBookID int64
}

// Created reports whether this call made the row the book's: a fresh insert
// or a reclaimed orphan. A same-book no-op is not a creation.
func (r TrackResult) Created() bool {
	return r.Outcome == TrackInserted || r.Outcome == TrackReclaimedOrphan
}

// Track records path against bookID and reports what happened. A path the
// same book already tracks is a no-op success, a row left behind by a deleted
// book is taken over, and a path another existing book tracks fails with
// *PathOwnedError and leaves that book's row alone (#2937).
//
// The insert, orphan takeover, former-variant reconciliation, and new
// owner's compatibility/status refresh share one transaction. Refresh may
// stat tracked paths but never changes files; all SQL stays on tx because the
// pool has one connection.
func (r *BookFileRepo) Track(ctx context.Context, bookID int64, format, path string) (TrackResult, error) {
	format = models.BookFileFormatFor(path, format)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return TrackResult{}, fmt.Errorf("book_files track begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO book_files (book_id, format, path, size_bytes, created_at)
		 VALUES (?, ?, ?, 0, ?)`,
		bookID, format, path, time.Now().UTC())
	if err != nil {
		return TrackResult{}, fmt.Errorf("book_files add: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return TrackResult{}, fmt.Errorf("book_files add rows: %w", err)
	}
	result := TrackResult{Outcome: TrackInserted}
	if n == 0 {
		var owner int64
		var ownerTitle sql.NullString
		var oldVariant sql.NullInt64
		err = tx.QueryRowContext(ctx,
			`SELECT bf.book_id, b.title, bf.variant_id FROM book_files bf LEFT JOIN books b ON b.id = bf.book_id
			 WHERE bf.path = ?`, path).Scan(&owner, &ownerTitle, &oldVariant)
		if errors.Is(err, sql.ErrNoRows) {
			return TrackResult{}, fmt.Errorf("book_files add: insert of %s was ignored but no row holds the path", path)
		}
		if err != nil {
			return TrackResult{}, fmt.Errorf("book_files owner lookup: %w", err)
		}
		switch {
		case owner == bookID:
			result = TrackResult{Outcome: TrackAlreadyTracked}
		case !ownerTitle.Valid:
			if _, err := tx.ExecContext(ctx,
				`UPDATE book_files SET book_id = ?, format = ?, variant_id = NULL, language = '', verification = 'unverified' WHERE path = ?`, bookID, format, path); err != nil {
				return TrackResult{}, fmt.Errorf("book_files reclaim orphaned row: %w", err)
			}
			if err := reconcileVariantAfterDetach(ctx, tx, oldVariant); err != nil {
				return TrackResult{}, err
			}
			result = TrackResult{Outcome: TrackReclaimedOrphan, PreviousBookID: owner}
		default:
			return TrackResult{}, &PathOwnedError{Path: path, OwnerBookID: owner, OwnerTitle: ownerTitle.String}
		}
	}
	if err := NewBookRepo(r.db).WithTx(tx).refreshBookStatus(ctx, bookID); err != nil {
		return TrackResult{}, fmt.Errorf("book_files refresh owner: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return TrackResult{}, fmt.Errorf("book_files track commit: %w", err)
	}
	return result, nil
}

// Add records path against bookID. It is Track without the outcome: a
// same-book re-add succeeds, and a path another book owns returns
// *PathOwnedError instead of being silently ignored (#2937).
func (r *BookFileRepo) Add(ctx context.Context, bookID int64, format, path string) error {
	_, err := r.Track(ctx, bookID, format, path)
	return err
}

// VariantReviewApproval identifies a named admin's exact audit for one
// reviewed download. The DB checks the event and download again in the same
// transaction that moves review to imported; a caller's boolean is not proof.
type VariantReviewApproval struct {
	AuditEventID, DownloadID int64
	ExpectedStatus           string
}

// TrackVariant records a successfully placed path and its language evidence in
// one transaction. The caller must finish private staging and collision-safe
// placement before calling; this method never touches a public or source file.
// A legacy path or another variant's path cannot be relabeled by an import.
// Review requires an explicit manual retry before a new claim is accepted.
func (r *BookFileRepo) TrackVariant(ctx context.Context, bookID, variantID int64, format, path, language, verification string) error {
	return r.TrackVariantChecked(ctx, bookID, variantID, format, path, language, verification, nil, VariantReviewApproval{})
}

// VariantFileClaim keeps each placed file's own language and verification
// evidence, even when several files belong to the same requested variant.
type VariantFileClaim struct {
	VariantID              int64
	Format, Path           string
	Language, Verification string
	Approval               VariantReviewApproval
}

// TrackVariantChecked retains the single-file API for existing callers.
func (r *BookFileRepo) TrackVariantChecked(ctx context.Context, bookID, variantID int64, format, path, language, verification string, validate func() error, approval VariantReviewApproval) error {
	return r.TrackVariantFilesChecked(ctx, bookID, []VariantFileClaim{{
		VariantID: variantID, Format: format, Path: path, Language: language, Verification: verification, Approval: approval,
	}}, validate)
}

// TrackVariantFilesChecked commits every file claim and variant status as one
// batch. The caller's check revalidates all public paths and original sources
// after the SQL writes and immediately before commit. Bindery must be the only
// library writer during import: SQLite cannot make a filesystem replacement
// atomic with its commit without a separate filesystem access-control gate.
func (r *BookFileRepo) TrackVariantFilesChecked(ctx context.Context, bookID int64, claims []VariantFileClaim, validate func() error) error {
	if bookID <= 0 || len(claims) == 0 {
		return fmt.Errorf("variant batch needs a book and at least one file")
	}
	seenPaths := make(map[string]bool, len(claims))
	for _, claim := range claims {
		if claim.VariantID <= 0 || !filepath.IsAbs(claim.Path) || filepath.Clean(claim.Path) != claim.Path ||
			claim.Path == string(filepath.Separator) || seenPaths[claim.Path] {
			return fmt.Errorf("invalid or duplicate variant path %q", claim.Path)
		}
		seenPaths[claim.Path] = true
		if claim.Format != models.MediaTypeEbook && claim.Format != models.MediaTypeAudiobook ||
			models.BookFileFormatFor(claim.Path, claim.Format) != claim.Format {
			return fmt.Errorf("invalid variant file format %q for %q", claim.Format, claim.Path)
		}
		if claim.Verification != "verified" && claim.Verification != "unverified" {
			return fmt.Errorf("invalid file verification %q", claim.Verification)
		}
		if claim.Language != "" && claim.Language != "eng" && claim.Language != "rum" && claim.Language != "fre" ||
			claim.Verification == "verified" && claim.Language == "" {
			return fmt.Errorf("invalid file language %q for %q verification", claim.Language, claim.Verification)
		}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("book_files track variant begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	type variantState struct {
		status   string
		approval VariantReviewApproval
	}
	states := make(map[int64]variantState, len(claims))
	variantIDs := make([]int64, 0, len(claims))
	for _, claim := range claims {
		var variantBookID int64
		var variantFormat, variantLanguage, status string
		if err := tx.QueryRowContext(ctx, `SELECT book_id, media_type, language, status FROM book_variants WHERE id = ?`, claim.VariantID).
			Scan(&variantBookID, &variantFormat, &variantLanguage, &status); err != nil {
			return fmt.Errorf("book_files variant lookup: %w", err)
		}
		if variantBookID != bookID || variantFormat != claim.Format ||
			claim.Verification == "verified" && claim.Language != variantLanguage ||
			claim.Approval.ExpectedStatus != "" && status != claim.Approval.ExpectedStatus {
			return fmt.Errorf("book_files variant %d does not match book, format, verified language or expected status", claim.VariantID)
		}
		if prior, ok := states[claim.VariantID]; ok {
			if prior.status != status || prior.approval != claim.Approval {
				return fmt.Errorf("book_files variant %d has inconsistent batch approval", claim.VariantID)
			}
		} else {
			if status == models.VariantReview {
				if err := validateVariantReviewApproval(ctx, tx, bookID, claim.VariantID, claim.Approval); err != nil {
					return fmt.Errorf("book_files variant %d remains in review: %w", claim.VariantID, err)
				}
			}
			states[claim.VariantID] = variantState{status, claim.Approval}
			variantIDs = append(variantIDs, claim.VariantID)
		}

		var owner, existingVariant sql.NullInt64
		var ownerTitle, existingLanguage, existingVerification sql.NullString
		var existingFormat string
		err = tx.QueryRowContext(ctx, `SELECT f.book_id, b.title, f.variant_id, f.language, f.verification, f.format
			FROM book_files f LEFT JOIN books b ON b.id = f.book_id WHERE f.path = ?`, claim.Path).
			Scan(&owner, &ownerTitle, &existingVariant, &existingLanguage, &existingVerification, &existingFormat)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `INSERT INTO book_files
				(book_id, format, path, size_bytes, created_at, variant_id, language, verification)
				VALUES (?, ?, ?, 0, ?, ?, ?, ?)`, bookID, claim.Format, claim.Path, time.Now().UTC(), claim.VariantID, claim.Language, claim.Verification); err != nil {
				return fmt.Errorf("book_files track variant insert: %w", err)
			}
		case err != nil:
			return fmt.Errorf("book_files track variant path lookup: %w", err)
		case owner.Int64 != bookID:
			return &PathOwnedError{Path: claim.Path, OwnerBookID: owner.Int64, OwnerTitle: ownerTitle.String}
		case existingVariant.Valid && existingVariant.Int64 == claim.VariantID && existingFormat == claim.Format &&
			existingLanguage.String == claim.Language && existingVerification.String == claim.Verification:
			// The already-tracked file keeps its evidence on an exact retry.
		default:
			return fmt.Errorf("book_files path %q already has a different or legacy claim", claim.Path)
		}
	}

	for _, variantID := range variantIDs {
		updated, err := tx.ExecContext(ctx, `UPDATE book_variants SET status = 'imported' WHERE id = ? AND status = ?`, variantID, states[variantID].status)
		if err != nil {
			return fmt.Errorf("book_files mark variant imported: %w", err)
		}
		if count, err := updated.RowsAffected(); err != nil || count != 1 {
			return fmt.Errorf("book_files variant status changed before import: %v", err)
		}
	}
	// Refresh the compatibility columns only after all file claims are present.
	if err := NewBookRepo(r.db).WithTx(tx).refreshBookStatus(ctx, bookID); err != nil {
		return fmt.Errorf("book_files refresh variant book: %w", err)
	}
	if validate != nil {
		if err := validate(); err != nil {
			return fmt.Errorf("book_files variant ownership changed before commit: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("book_files track variant commit: %w", err)
	}
	return nil
}

func validateVariantReviewApproval(ctx context.Context, tx *sql.Tx, bookID, variantID int64, approval VariantReviewApproval) error {
	if approval.ExpectedStatus != models.VariantReview || approval.AuditEventID <= 0 || approval.DownloadID <= 0 {
		return fmt.Errorf("missing exact reviewed-download approval")
	}
	var data string
	if err := tx.QueryRowContext(ctx, `SELECT data FROM history WHERE id = ? AND book_id = ? AND event_type = 'variantLanguageOverride'`,
		approval.AuditEventID, bookID).Scan(&data); err != nil {
		return fmt.Errorf("load review audit: %w", err)
	}
	var event struct {
		ActorUserID int64  `json:"actorUserId"`
		VariantID   int64  `json:"variantId"`
		DownloadID  int64  `json:"downloadId"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil || event.ActorUserID <= 0 ||
		event.VariantID != variantID || event.DownloadID != approval.DownloadID || strings.TrimSpace(event.Reason) == "" {
		return fmt.Errorf("review audit does not approve this variant and download: %v", err)
	}
	var linkedBook, linkedVariant sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT book_id, variant_id FROM downloads WHERE id = ?`, approval.DownloadID).
		Scan(&linkedBook, &linkedVariant); err != nil || !linkedBook.Valid || linkedBook.Int64 != bookID ||
		!linkedVariant.Valid || linkedVariant.Int64 != variantID {
		return fmt.Errorf("review audit download association changed: %v", err)
	}
	return nil
}

// SetVariant changes verification on a file already associated with the same
// variant, or clears its association. Only TrackVariant may attach a new
// variant after safe placement. The language evidence must match the existing
// variant, and clearing or changing verification reconciles its status.
func (r *BookFileRepo) SetVariant(ctx context.Context, path string, variantID *int64, language, verification string) error {
	if verification != "verified" && verification != "unverified" ||
		(language != "" && language != "eng" && language != "rum" && language != "fre") ||
		verification == "verified" && language == "" {
		return fmt.Errorf("invalid variant file language %q or verification %q", language, verification)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("book_files set variant begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var oldVariant sql.NullInt64
	var oldVerification string
	if err := tx.QueryRowContext(ctx, `SELECT variant_id, verification FROM book_files WHERE path = ?`, path).Scan(&oldVariant, &oldVerification); err != nil {
		return fmt.Errorf("book_files set variant lookup: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE book_files
		SET variant_id = ?, language = ?, verification = ?
		WHERE path = ? AND (? IS NULL OR (book_files.variant_id = ? AND EXISTS (
			SELECT 1 FROM book_variants v
			WHERE v.id = ? AND v.book_id = book_files.book_id AND v.media_type = book_files.format
			AND ((? = '' AND ? = 'unverified') OR v.language = ?)
		)))`, variantID, language, verification, path, variantID, variantID, variantID, language, verification, language)
	if err != nil {
		return fmt.Errorf("book_files set variant: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("book_files set variant rows: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("book_files set variant: path %q missing or variant does not match its book, format, or language", path)
	}
	if oldVariant.Valid && (variantID == nil || oldVariant.Int64 != *variantID || oldVerification != verification) {
		if err := reconcileVariantAfterDetach(ctx, tx, oldVariant); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("book_files set variant commit: %w", err)
	}
	return nil
}

// AddIfMissing records a new on-disk file and reports whether this call is the
// one that made the row the book's (see TrackResult.Created). A path the book
// already tracks comes back false; a path another book owns comes back as
// *PathOwnedError rather than being stolen or reported as handled.
//
// Callers that need to know what they own use this instead of Add: the Calibre
// importer may only claim, and later roll back, a file row it actually created
// (#1635), mirroring SeriesRepo.LinkBookIfMissing.
func (r *BookFileRepo) AddIfMissing(ctx context.Context, bookID int64, format, path string) (bool, error) {
	res, err := r.Track(ctx, bookID, format, path)
	if err != nil {
		return false, err
	}
	return res.Created(), nil
}

// MoveToBook makes bookID the owner of path, whoever held it before, and
// returns the previous owner (0 when the path was untracked, bookID when it
// was already the book's). It is for the explicit "this file belongs to that
// book" action (Fix match, #1238), never for an ordinary import, which must
// not take a file from another book (#2937). Runs in one transaction.
func (r *BookFileRepo) MoveToBook(ctx context.Context, bookID int64, format, path string) (int64, error) {
	format = models.BookFileFormatFor(path, format)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("book_files move begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	prev, err := r.moveToBookTx(ctx, tx, bookID, format, path)
	if err != nil {
		return 0, err
	}
	books := NewBookRepo(r.db).WithTx(tx)
	if prev != 0 && prev != bookID {
		if err := books.refreshBookStatus(ctx, prev); err != nil {
			return 0, err
		}
	}
	if err := books.refreshBookStatus(ctx, bookID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("book_files move commit: %w", err)
	}
	return prev, nil
}

// moveToBookTx lets BookRepo commit the ownership change with both books'
// compatibility views. No SQL leaves the caller's transaction.
func (r *BookFileRepo) moveToBookTx(ctx context.Context, tx *sql.Tx, bookID int64, format, path string) (int64, error) {
	format = models.BookFileFormatFor(path, format)
	var prev int64
	var oldVariant sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT book_id, variant_id FROM book_files WHERE path = ?`, path).Scan(&prev, &oldVariant)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO book_files (book_id, format, path, size_bytes, created_at) VALUES (?, ?, ?, 0, ?)`,
			bookID, format, path, time.Now().UTC()); err != nil {
			return 0, fmt.Errorf("book_files move insert: %w", err)
		}
	case err != nil:
		return 0, fmt.Errorf("book_files move lookup: %w", err)
	case prev == bookID:
		return prev, nil
	default:
		if _, err := tx.ExecContext(ctx,
			`UPDATE book_files SET book_id = ?, format = ?, variant_id = NULL, language = '', verification = 'unverified' WHERE path = ?`, bookID, format, path); err != nil {
			return 0, fmt.Errorf("book_files move update: %w", err)
		}
		if err := reconcileVariantAfterDetach(ctx, tx, oldVariant); err != nil {
			return 0, err
		}
	}
	return prev, nil
}

// UpdatePath changes the on-disk path of the book_files row with the given id.
// Used by the library reorganize action (#1181) after a tracked file is moved
// to the location the current naming template computes. The path column is
// globally UNIQUE, so a move onto a path another row already owns fails here
// rather than silently corrupting the index.
func (r *BookFileRepo) UpdatePath(ctx context.Context, id int64, newPath string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE book_files SET path = ? WHERE id = ?`, newPath, id)
	if err != nil {
		return fmt.Errorf("book_files update path: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("book_files update path rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("book_files update path: no row with id %d", id)
	}
	pathEpochFor(r.db).Add(1)
	return nil
}

// BookIDForFile returns the owning book_id for a book_files row id, or 0 when
// no such row exists. Used by the reorganize apply path (#1181) to resolve a
// file the client picked back to its book.
func (r *BookFileRepo) BookIDForFile(ctx context.Context, fileID int64) (int64, error) {
	var bookID int64
	err := r.db.QueryRowContext(ctx,
		`SELECT book_id FROM book_files WHERE id = ?`, fileID).Scan(&bookID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("book_files book_id lookup: %w", err)
	}
	return bookID, nil
}

// GetByPath returns the book_files row recorded at path, or nil when no row
// has that exact path.
func (r *BookFileRepo) GetByPath(ctx context.Context, path string) (*models.BookFile, error) {
	var f models.BookFile
	err := r.db.QueryRowContext(ctx,
		`SELECT id, book_id, format, path, size_bytes, created_at, variant_id, language, verification
		 FROM book_files WHERE path = ?`, path).
		Scan(&f.ID, &f.BookID, &f.Format, &f.Path, &f.SizeBytes, &f.CreatedAt, &f.VariantID, &f.Language, &f.Verification)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("book_files get by path: %w", err)
	}
	return &f, nil
}

// ListByBook returns all book_files rows for the given book, ordered by id.
func (r *BookFileRepo) ListByBook(ctx context.Context, bookID int64) ([]models.BookFile, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, book_id, format, path, size_bytes, created_at, variant_id, language, verification
		 FROM book_files WHERE book_id = ? ORDER BY id`,
		bookID)
	if err != nil {
		return nil, fmt.Errorf("book_files list: %w", err)
	}
	defer rows.Close()

	var files []models.BookFile
	for rows.Next() {
		var f models.BookFile
		if err := rows.Scan(&f.ID, &f.BookID, &f.Format, &f.Path, &f.SizeBytes, &f.CreatedAt, &f.VariantID, &f.Language, &f.Verification); err != nil {
			return nil, fmt.Errorf("book_files scan: %w", err)
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

// ListByBooks returns every book_files row for any of the given book IDs, in
// one query, grouped by book_id. Used by the manual-import scan
// (ManualImportHandler.Scan, #2480) to replace an N+1 ListByBook call per
// confident catalogue match with a single round trip. Duplicate IDs collapse
// naturally (IN ignores repeats); an empty bookIDs returns an empty map.
func (r *BookFileRepo) ListByBooks(ctx context.Context, bookIDs []int64) (map[int64][]models.BookFile, error) {
	result := make(map[int64][]models.BookFile, len(bookIDs))
	if len(bookIDs) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(bookIDs))
	args := make([]any, len(bookIDs))
	for i, id := range bookIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `SELECT id, book_id, format, path, size_bytes, created_at, variant_id, language, verification
		FROM book_files WHERE book_id IN (` + strings.Join(placeholders, ",") + `) ORDER BY id` // #nosec G202 -- placeholders are generated from fixed ? tokens; book IDs remain bound args
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("book_files list by books: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var f models.BookFile
		if err := rows.Scan(&f.ID, &f.BookID, &f.Format, &f.Path, &f.SizeBytes, &f.CreatedAt, &f.VariantID, &f.Language, &f.Verification); err != nil {
			return nil, fmt.Errorf("book_files scan: %w", err)
		}
		result[f.BookID] = append(result[f.BookID], f)
	}
	return result, rows.Err()
}

// DeleteByBook removes a book's file rows and reconciles every affected
// variant before committing. It does not remove anything from disk.
func (r *BookFileRepo) DeleteByBook(ctx context.Context, bookID int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("book_files delete by book begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT variant_id FROM book_files WHERE book_id = ? AND variant_id IS NOT NULL`, bookID)
	if err != nil {
		return fmt.Errorf("book_files delete by book variants: %w", err)
	}
	var affected []sql.NullInt64
	for rows.Next() {
		var id sql.NullInt64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("book_files delete by book scan: %w", err)
		}
		affected = append(affected, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("book_files delete by book rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM book_files WHERE book_id = ?`, bookID); err != nil {
		return fmt.Errorf("book_files delete by book: %w", err)
	}
	for _, id := range affected {
		if err := reconcileVariantAfterDetach(ctx, tx, id); err != nil {
			return err
		}
	}
	if err := NewBookRepo(r.db).WithTx(tx).refreshBookStatus(ctx, bookID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("book_files delete by book commit: %w", err)
	}
	return nil
}

// DeleteByPath removes a tracked path and reconciles its variant in the same
// transaction, returning the book id (0 when the path was not tracked).
func (r *BookFileRepo) DeleteByPath(ctx context.Context, path string) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("book_files delete by path begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var bookID int64
	var variantID sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT book_id, variant_id FROM book_files WHERE path = ?`, path).Scan(&bookID, &variantID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("book_files lookup by path: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM book_files WHERE path = ?`, path); err != nil {
		return 0, fmt.Errorf("book_files delete by path: %w", err)
	}
	if err := reconcileVariantAfterDetach(ctx, tx, variantID); err != nil {
		return 0, err
	}
	if err := NewBookRepo(r.db).WithTx(tx).refreshBookStatus(ctx, bookID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("book_files delete by path commit: %w", err)
	}
	return bookID, nil
}

// PathOwnedByOtherBook reports whether the given on-disk path is present in
// book_files under a book other than excludeBookID. The path column is globally
// UNIQUE, so there is at most one owner. Pass excludeBookID=0 to treat ANY
// registered owner as "another book" (e.g. when the current book's rows have
// already been cascade-deleted). The delete and reassign-cleanup paths use this
// to avoid os.Remove-ing a file another book still owns (#1368). A row left by
// a deleted book counts as owned here on purpose; PathOwnedByLiveOtherBook is
// the variant that does not.
func (r *BookFileRepo) PathOwnedByOtherBook(ctx context.Context, path string, excludeBookID int64) (bool, error) {
	var owner int64
	err := r.db.QueryRowContext(ctx, `SELECT book_id FROM book_files WHERE path = ? LIMIT 1`, path).Scan(&owner)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("book_files owner lookup: %w", err)
	}
	return owner != excludeBookID, nil
}

// PathOwnedByLiveOtherBook is PathOwnedByOtherBook counting only an owner that
// still exists. A row left by a deleted book (foreign keys lost, #1727) is
// owned by nobody here, because Track takes such a row over (#2937).
//
// Only a check that guards a write through Track may use this: adoption's
// ownership check and the existing file bind when an author is added. A
// delete must keep using PathOwnedByOtherBook, where an orphan row counts as
// owned: refusing to unlink is the safe answer when ownership is unclear.
func (r *BookFileRepo) PathOwnedByLiveOtherBook(ctx context.Context, path string, excludeBookID int64) (bool, error) {
	var owner int64
	err := r.db.QueryRowContext(ctx,
		`SELECT bf.book_id FROM book_files bf JOIN books b ON b.id = bf.book_id
		 WHERE bf.path = ? LIMIT 1`, path).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("book_files live owner lookup: %w", err)
	}
	return owner != excludeBookID, nil
}

// PathUnderOtherBookDir reports whether a strict ancestor directory of path is
// itself registered in book_files by a book other than excludeBookID. An
// audiobook is often recorded as its whole folder, and that row covers every
// file below it at any depth, so a file there is not free to delete just
// because no row names it exactly (#3336).
//
// Ancestors are resolved lexically, and a stored row may carry a trailing
// separator because Track stores a path as given, so each ancestor is matched
// with and without one. Case and Windows style separators are not folded: a
// case insensitive or SMB library that spells the same folder two ways is not
// covered.
//
// A row left by a deleted book still counts, like PathOwnedByOtherBook: this
// guards deletes, where refusing to unlink is the safe answer when ownership
// is unclear.
func (r *BookFileRepo) PathUnderOtherBookDir(ctx context.Context, path string, excludeBookID int64) (bool, error) {
	var forms []any
	for dir := filepath.Dir(filepath.Clean(path)); ; dir = filepath.Dir(dir) {
		if dir == "." || dir == string(filepath.Separator) || dir == filepath.Dir(dir) {
			break
		}
		forms = append(forms, dir, dir+string(filepath.Separator))
	}
	if len(forms) == 0 {
		return false, nil
	}
	query := `SELECT book_id FROM book_files
		 WHERE book_id != ? AND path IN (?` + strings.Repeat(",?", len(forms)-1) + `) LIMIT 1`
	var owner int64
	err := r.db.QueryRowContext(ctx, query, append([]any{excludeBookID}, forms...)...).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("book_files ancestor owner lookup: %w", err)
	}
	return true, nil
}

// LiveOwnerOfPath returns the id of the existing book that tracks path, or 0
// when no live book does. A row left by a deleted book does not count, for the
// reason PathOwnedByLiveOtherBook gives.
func (r *BookFileRepo) LiveOwnerOfPath(ctx context.Context, path string) (int64, error) {
	var owner int64
	err := r.db.QueryRowContext(ctx,
		`SELECT bf.book_id FROM book_files bf JOIN books b ON b.id = bf.book_id
		 WHERE bf.path = ? LIMIT 1`, path).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("book_files live owner lookup: %w", err)
	}
	return owner, nil
}

// ListAllPaths returns every path currently registered in book_files.
// Used by ScanLibrary to build the set of already-tracked files.
func (r *BookFileRepo) ListAllPaths(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT path FROM book_files`)
	if err != nil {
		return nil, fmt.Errorf("book_files list all paths: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("book_files scan path: %w", err)
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// RecentEbookPaths returns up to limit ebook paths, newest first. The Calibre
// Test connection walks them for one that exists on Bindery's side and asks
// the plugin to open it through the push remap, because the library root on
// its own is an exact prefix match that never exercises the remap's join
// (#2831).
func (r *BookFileRepo) RecentEbookPaths(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT path FROM book_files WHERE format = 'ebook' ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("book_files recent ebook paths: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("book_files scan recent ebook path: %w", err)
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}
