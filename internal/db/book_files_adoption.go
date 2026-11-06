package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// UntrackFilePathForBook removes the book_files row for path only while it
// still belongs to bookID, then refreshes that book's status. It reports
// whether a row was removed. The file on disk is never touched.
//
// Library adoption's Undo uses it instead of UntrackFilePath: between the adopt
// and the undo the path may have been deleted with its book and registered to
// another one, and that book's row must survive the undo.
func (r *BookRepo) UntrackFilePathForBook(ctx context.Context, path string, bookID int64) (bool, error) {
	if _, inTx := r.exec.(*sql.Tx); !inTx {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return false, fmt.Errorf("untrack file path for book begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		removed, err := r.WithTx(tx).UntrackFilePathForBook(ctx, path, bookID)
		if err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("untrack file path for book commit: %w", err)
		}
		return removed, nil
	}
	var owner int64
	var variantID sql.NullInt64
	err := r.exec.QueryRowContext(ctx, `SELECT book_id, variant_id FROM book_files WHERE path = ?`, path).Scan(&owner, &variantID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && owner != bookID {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("untrack file path for book lookup: %w", err)
	}
	out, err := r.exec.ExecContext(ctx, `DELETE FROM book_files WHERE path = ? AND book_id = ?`, path, bookID)
	if err != nil {
		return false, fmt.Errorf("untrack file path for book: %w", err)
	}
	n, err := out.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if err := reconcileVariantAfterDetach(ctx, r.exec.(*sql.Tx), variantID); err != nil {
		return false, err
	}
	if err := r.refreshBookStatus(ctx, bookID); err != nil {
		return false, err
	}
	return true, nil
}
