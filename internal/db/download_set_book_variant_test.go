package db

import (
	"context"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// A book reassignment must not turn a reviewed torrent into an unbound legacy
// download. Otherwise Match to book can reset its import retry without the
// variant's source rejection or content guard.
func TestSetBookIDRefusesCrossBookReviewedVariantWithoutMutation(t *testing.T) {
	variants, repo, book, variant, dl := reviewedTorrentImportFixture(t)
	ctx := context.Background()
	other := &models.Book{ForeignID: "cross-book-review-target", AuthorID: book.AuthorID,
		Title: "Other work", SortTitle: "other work", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := NewBookRepo(repo.db).Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	const source = "/seed/private/wrong-work.epub"
	if err := NewVariantRejectionRepo(repo.db).Record(ctx, book.ID, variant.ID, dl.ID, source, "wrong work"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetImportPath(ctx, dl.ID, source); err != nil {
		t.Fatal(err)
	}
	before, err := repo.GetByID(ctx, dl.ID)
	if err != nil || before == nil {
		t.Fatalf("read reviewed torrent: %+v %v", before, err)
	}
	if err := repo.SetBookID(ctx, dl.ID, other.ID); err == nil || !strings.Contains(err.Error(), "variant") {
		t.Fatalf("cross-book SetBookID = %v, want variant reassignment error", err)
	}
	got, err := repo.GetByID(ctx, dl.ID)
	if err != nil || got == nil || got.BookID == nil || *got.BookID != book.ID || got.VariantID == nil || *got.VariantID != variant.ID ||
		got.Status != before.Status || got.ImportRetryCount != before.ImportRetryCount || got.ImportPath != before.ImportPath ||
		got.TorrentID == nil || *got.TorrentID != *before.TorrentID || !strings.Contains(got.ErrorMessage, "file list unavailable") {
		t.Fatalf("rejected reassignment altered reviewed download: %+v %v", got, err)
	}
	if status := terminalVariantStatus(t, variants, book.ID); status != models.VariantReview {
		t.Fatalf("review variant changed to %q", status)
	}
	if rejected, err := NewVariantRejectionRepo(repo.db).IsRejected(ctx, book.ID, variant.ID, source); err != nil || !rejected {
		t.Fatalf("rejection was lost: %t %v", rejected, err)
	}
	if accepted, found, err := repo.ResetImportRetry(ctx, dl.ID); err != nil || accepted || !found {
		t.Fatalf("generic retry bypassed review: accepted=%t found=%t err=%v", accepted, found, err)
	}
}

func TestSetBookIDSameBookKeepsVariantAssociation(t *testing.T) {
	variants, repo, book, variant, dl := reviewedTorrentImportFixture(t)
	ctx := context.Background()
	if err := repo.SetBookID(ctx, dl.ID, book.ID); err != nil {
		t.Fatalf("idempotent SetBookID: %v", err)
	}
	got, err := repo.GetByID(ctx, dl.ID)
	if err != nil || got == nil || got.BookID == nil || *got.BookID != book.ID || got.VariantID == nil || *got.VariantID != variant.ID || got.Status != models.StateImportBlocked {
		t.Fatalf("same-book call changed reviewed association: %+v %v", got, err)
	}
	if status := terminalVariantStatus(t, variants, book.ID); status != models.VariantReview {
		t.Fatalf("idempotent call changed variant state to %q", status)
	}
}

func TestSetBookIDAllowsUnboundLegacyCrossBookMatch(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	other := &models.Book{ForeignID: "legacy-match-target", AuthorID: book.AuthorID,
		Title: "Other work", SortTitle: "other work", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := NewBookRepo(database).Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	repo := NewDownloadRepo(database)
	dl := &models.Download{GUID: "legacy-match", BookID: &book.ID, Title: "Legacy download", Status: models.StateImportBlocked, Protocol: "usenet"}
	if err := repo.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetBookID(ctx, dl.ID, other.ID); err != nil {
		t.Fatalf("legacy Match to book: %v", err)
	}
	got, err := repo.GetByID(ctx, dl.ID)
	if err != nil || got == nil || got.BookID == nil || *got.BookID != other.ID || got.VariantID != nil || got.Status != models.StateImportBlocked {
		t.Fatalf("legacy download not reassigned: %+v %v", got, err)
	}
}
