package db

import (
	"context"
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
		got, err := repo.IsRejected(ctx, tc.book, tc.path)
		if err != nil || got != tc.want {
			t.Errorf("IsRejected(%d, %q) = %t, %v; want %t", tc.book, tc.path, got, err, tc.want)
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
	if rejected, err := repo.IsRejected(ctx, book.ID, "/downloads/mam/wrong.epub"); err != nil || rejected {
		t.Errorf("invalid association wrote a row: %t, %v", rejected, err)
	}
}

func TestVariantRejectionsFailClosedOnReadError(t *testing.T) {
	repo, book, _, _ := rejectionFixture(t)
	if err := repo.db.Close(); err != nil {
		t.Fatal(err)
	}
	if rejected, err := repo.IsRejected(context.Background(), book.ID, "/downloads/mam/dune.epub"); err == nil || rejected {
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
	if rejected, err := NewVariantRejectionRepo(again).IsRejected(ctx, book.ID, source); err != nil || !rejected {
		t.Fatalf("retry after reopen lost rejection: %t, %v", rejected, err)
	}
}
