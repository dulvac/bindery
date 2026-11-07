package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

func rejectionFixture(t *testing.T) (*VariantRejectionRepo, *models.Book, int64, int64) {
	t.Helper()
	database, _, book := openTestDB(t)
	ctx := context.Background()
	variants, err := NewBookVariantRepo(database).EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	dl := &models.Download{GUID: "rejection-fixture", BookID: &book.ID, Status: models.StateImported, Title: book.Title}
	if err := NewDownloadRepo(database).Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	return NewVariantRejectionRepo(database), book, variants[0].ID, dl.ID
}

// A retry may record the same source twice; it must not change its reason or
// create duplicate rows. Another book/source pair remains independent.
func TestVariantRejectionsIdempotentAndScoped(t *testing.T) {
	repo, book, variantID, downloadID := rejectionFixture(t)
	ctx := context.Background()
	path := "/downloads/mam/dune.epub"
	if err := repo.Record(ctx, book.ID, variantID, downloadID, path, "wrong language"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Record(ctx, book.ID, variantID, downloadID, path, "different reason"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		book int64
		path string
		want bool
	}{
		{book.ID, path, true},
		{book.ID + 1, path, false},
		{book.ID, "/downloads/mam/english.epub", false},
	} {
		got, err := repo.IsRejected(ctx, tc.book, variantID, tc.path)
		if err != nil || got != tc.want {
			t.Errorf("IsRejected(%d, %d, %q) = %t, %v; want %t", tc.book, variantID, tc.path, got, err, tc.want)
		}
	}
	var count int
	var reason string
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(reason) FROM variant_rejections WHERE book_id = ? AND source_path = ?`, book.ID, path).Scan(&count, &reason); err != nil {
		t.Fatal(err)
	}
	if count != 1 || reason != "wrong language" {
		t.Fatalf("repeat changed rejection: count=%d reason=%q", count, reason)
	}
}

// A source that missed one language must be eligible for a different variant.
// A second independent verdict must also be retained for that variant.
func TestVariantRejectionsKeepIndependentVerdictsForSameSource(t *testing.T) {
	repo, book, englishID, downloadID := rejectionFixture(t)
	ctx := context.Background()
	romanian, err := NewBookVariantRepo(repo.db).EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"rum"})
	if err != nil || len(romanian) != 1 {
		t.Fatalf("Romanian variant: %+v %v", romanian, err)
	}
	const path = "/downloads/mam/dune.epub"
	if err := repo.Record(ctx, book.ID, englishID, downloadID, path, "wrong language for English"); err != nil {
		t.Fatal(err)
	}
	if rejected, err := repo.IsRejected(ctx, book.ID, romanian[0].ID, path); err != nil || rejected {
		t.Fatalf("English verdict blocks Romanian before reassessment: %t %v", rejected, err)
	}
	if err := repo.Record(ctx, book.ID, romanian[0].ID, downloadID, path, "wrong work for Romanian"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM variant_rejections WHERE book_id = ? AND source_path = ?`, book.ID, path).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("same source has %d verdicts, want one per variant", count)
	}
	for _, id := range []int64{englishID, romanian[0].ID} {
		if rejected, err := repo.IsRejected(ctx, book.ID, id, path); err != nil || !rejected {
			t.Fatalf("variant %d lost its rejection: %t %v", id, rejected, err)
		}
	}
}

func TestVariantRejectionsRefuseWrongAssociations(t *testing.T) {
	repo, book, variantID, downloadID := rejectionFixture(t)
	ctx := context.Background()
	cases := []struct {
		book, variant, download int64
	}{
		{book.ID + 100, variantID, downloadID},
		{book.ID, variantID + 100, downloadID},
		{book.ID, variantID, downloadID + 100},
	}
	for _, tc := range cases {
		if err := repo.Record(ctx, tc.book, tc.variant, tc.download, "/downloads/mam/wrong.epub", "wrong work"); !errors.Is(err, ErrVariantRejectionAssociation) {
			t.Errorf("Record(%+v) = %v; want association error", tc, err)
		}
	}
	// An existing valid book/variant and a download owned by a different book
	// cannot be combined, even when both referenced rows exist.
	other := &models.Book{ForeignID: "rejection-other-book", AuthorID: book.AuthorID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := NewBookRepo(repo.db).Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	foreign := &models.Download{GUID: "rejection-other-download", BookID: &other.ID, Title: other.Title, Status: models.StateImported}
	if err := NewDownloadRepo(repo.db).Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if err := repo.Record(ctx, book.ID, variantID, foreign.ID, "/downloads/mam/wrong.epub", "wrong work"); !errors.Is(err, ErrVariantRejectionAssociation) {
		t.Errorf("cross-book download = %v; want association error", err)
	}
	if rejected, err := repo.IsRejected(ctx, book.ID, variantID, "/downloads/mam/wrong.epub"); err != nil || rejected {
		t.Errorf("invalid association wrote a row: %t, %v", rejected, err)
	}
	otherVariant, err := NewBookVariantRepo(repo.db).EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET variant_id = ? WHERE id = ?`, variantID, downloadID); err != nil {
		t.Fatal(err)
	}
	const otherPath = "/downloads/mam/other-variant.epub"
	if err := repo.Record(ctx, book.ID, otherVariant[0].ID, downloadID, otherPath, "wrong variant"); !errors.Is(err, ErrVariantRejectionAssociation) {
		t.Fatalf("download bound to a different variant was accepted: %v", err)
	}
	if rejected, err := repo.IsRejected(ctx, book.ID, otherVariant[0].ID, otherPath); err != nil || rejected {
		t.Fatalf("wrong variant wrote a rejection: %t, %v", rejected, err)
	}
}

func TestVariantRejectionsKeepVerdictsWhenDownloadForgotten(t *testing.T) {
	repo, book, variantID, downloadID := rejectionFixture(t)
	ctx := context.Background()
	var foreignKeys int
	if err := repo.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("test requires SQLite FK enforcement: foreign_keys=%d, err=%v", foreignKeys, err)
	}
	const rejected = "/downloads/mam/rejected.epub"
	const unrelated = "/downloads/mam/unrelated.epub"
	const independent = "/downloads/mam/independent.epub"
	for _, source := range []string{rejected, unrelated} {
		if err := repo.Record(ctx, book.ID, variantID, downloadID, source, "wrong language"); err != nil {
			t.Fatal(err)
		}
	}
	other := &models.Download{GUID: "independent-rejection", BookID: &book.ID, Status: models.StateImported, Title: book.Title}
	if err := NewDownloadRepo(repo.db).Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := repo.Record(ctx, book.ID, variantID, other.ID, independent, "different verdict"); err != nil {
		t.Fatal(err)
	}
	created := make(map[string]string, 3)
	for _, source := range []string{rejected, unrelated, independent} {
		var stamp string
		if err := repo.db.QueryRowContext(ctx, `SELECT CAST(created_at AS TEXT) FROM variant_rejections WHERE source_path = ?`, source).Scan(&stamp); err != nil {
			t.Fatal(err)
		}
		created[source] = stamp
	}
	if err := NewDownloadRepo(repo.db).Delete(ctx, downloadID); err != nil {
		t.Fatalf("forget rejected download: %v", err)
	}
	if row, err := NewDownloadRepo(repo.db).GetByID(ctx, downloadID); err != nil || row != nil {
		t.Fatalf("forgotten download still exists: %+v, %v", row, err)
	}
	for _, source := range []string{rejected, unrelated} {
		var bookAfter, variantAfter int64
		var downloadAfter sql.NullInt64
		var pathAfter, reasonAfter, createdAfter string
		if err := repo.db.QueryRowContext(ctx, `SELECT book_id, variant_id, download_id, source_path, reason, CAST(created_at AS TEXT)
			FROM variant_rejections WHERE source_path = ?`, source).Scan(&bookAfter, &variantAfter, &downloadAfter, &pathAfter, &reasonAfter, &createdAfter); err != nil {
			t.Fatal(err)
		}
		if bookAfter != book.ID || variantAfter != variantID || downloadAfter.Valid || pathAfter != source || reasonAfter != "wrong language" || createdAfter != created[source] {
			t.Fatalf("forget changed verdict %q: book=%d variant=%d download=%v path=%q reason=%q created=%q", source, bookAfter, variantAfter, downloadAfter, pathAfter, reasonAfter, createdAfter)
		}
		if blocked, err := repo.IsRejected(ctx, book.ID, variantID, source); err != nil || !blocked {
			t.Fatalf("forgotten source %q is eligible again: blocked=%t err=%v", source, blocked, err)
		}
	}
	var keptDownload int64
	var keptReason, keptCreated string
	if err := repo.db.QueryRowContext(ctx, `SELECT download_id, reason, CAST(created_at AS TEXT) FROM variant_rejections WHERE source_path = ?`, independent).
		Scan(&keptDownload, &keptReason, &keptCreated); err != nil {
		t.Fatal(err)
	}
	if keptDownload != other.ID || keptReason != "different verdict" || keptCreated != created[independent] {
		t.Fatalf("unrelated rejection changed: download=%d reason=%q created=%q", keptDownload, keptReason, keptCreated)
	}
	if blocked, err := repo.IsRejected(ctx, book.ID, variantID, independent); err != nil || !blocked {
		t.Fatalf("unrelated rejection lost: %t, %v", blocked, err)
	}
	if err := repo.Record(ctx, book.ID, variantID, downloadID, "/downloads/mam/new.epub", "new verdict"); !errors.Is(err, ErrVariantRejectionAssociation) {
		t.Fatalf("forgotten download created a new rejection: %v", err)
	}
	rows, err := repo.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check found an orphan after forgetting the download")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestVariantRejectionsFailClosedOnReadError(t *testing.T) {
	repo, book, variantID, _ := rejectionFixture(t)
	if err := repo.db.Close(); err != nil {
		t.Fatal(err)
	}
	if rejected, err := repo.IsRejected(context.Background(), book.ID, variantID, "/downloads/mam/dune.epub"); err == nil || rejected {
		t.Fatalf("closed DB lookup = %t, %v; must return error, never a clean negative", rejected, err)
	}
}

func TestVariantRejectionsSurviveReopenForRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindery.sqlite")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	author := &models.Author{ForeignID: "reopen-a", Name: "Author", SortName: "Author"}
	if err := NewAuthorRepo(database).Create(ctx, author); err != nil {
		t.Fatal(err)
	}
	book := &models.Book{ForeignID: "reopen-b", AuthorID: author.ID, Title: "Book", SortTitle: "Book", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := NewBookRepo(database).Create(ctx, book); err != nil {
		t.Fatal(err)
	}
	variants, err := NewBookVariantRepo(database).EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	dl := &models.Download{GUID: "reopen-download", BookID: &book.ID, Title: book.Title, Status: models.StateImported}
	if err := NewDownloadRepo(database).Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	const source = "/downloads/mam/dune.epub"
	if err := NewVariantRejectionRepo(database).Record(ctx, book.ID, variants[0].ID, dl.ID, source, "wrong language"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if rejected, err := NewVariantRejectionRepo(again).IsRejected(ctx, book.ID, variants[0].ID, source); err != nil || !rejected {
		t.Fatalf("retry after reopen lost rejection: %t, %v", rejected, err)
	}
}
