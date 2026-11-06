package db

import (
	"context"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// A real grab reserves the variant and then marks it downloading. Starting
// these tests at wanted would miss the queue state that gets stranded.
func downloadingImportFixture(t *testing.T) (*BookVariantRepo, *DownloadRepo, *models.Book, *models.BookVariant, *models.Download) {
	t.Helper()
	database, _, book := openTestDB(t)
	ctx := context.Background()
	variants := NewBookVariantRepo(database)
	copies, err := variants.EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewDownloadRepo(database)
	dl := &models.Download{GUID: "terminal-import", BookID: &book.ID, VariantID: &copies[0].ID,
		Title: "Sample Book [ENG / EPUB]", Protocol: "torrent", Status: models.StateGrabbed}
	if err := repo.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateStatus(ctx, dl.ID, models.StateDownloading); err != nil {
		t.Fatal(err)
	}
	if marked, err := variants.MarkDownloading(ctx, copies[0].ID); err != nil || !marked {
		t.Fatalf("mark downloaded variant: %t %v", marked, err)
	}
	for _, next := range []models.DownloadState{models.StateCompleted, models.StateImportPending, models.StateImporting} {
		if err := repo.UpdateStatus(ctx, dl.ID, next); err != nil {
			t.Fatal(err)
		}
	}
	return variants, repo, book, &copies[0], dl
}

func terminalVariantStatus(t *testing.T, variants *BookVariantRepo, bookID int64) string {
	t.Helper()
	copies, err := variants.ListByBook(context.Background(), bookID)
	if err != nil || len(copies) != 1 {
		t.Fatalf("read variants: %+v %v", copies, err)
	}
	return copies[0].Status
}

func TestVariantTerminalImportBlockedReconcilesDownloadingToReview(t *testing.T) {
	variants, repo, book, _, dl := downloadingImportFixture(t)
	ctx := context.Background()
	if err := repo.SetErrorWithStatus(ctx, dl.ID, models.StateImportBlocked, "destination collision: review existing link"); err != nil {
		t.Fatal(err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantReview {
		t.Fatalf("blocked torrent left variant %q; want actionable review", got)
	}
	row, err := repo.GetByID(ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateImportBlocked || !strings.Contains(row.ErrorMessage, "destination collision") {
		t.Fatalf("blocked download lost its manual retry/reason: %+v %v", row, err)
	}
}

// A successful client send can outlive a failed MarkDownloading write. A
// terminal import must not leave that still-wanted copy free for another grab.
func TestVariantTerminalImportClosesUnmarkedGrabWithoutDuplicate(t *testing.T) {
	variants, repo, book, variant, dl := downloadingImportFixture(t)
	ctx := context.Background()
	if err := variants.SetStatus(ctx, variant.ID, models.VariantWanted); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetErrorWithStatus(ctx, dl.ID, models.StateImportBlocked, "cannot place file"); err != nil {
		t.Fatal(err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantReview {
		t.Fatalf("blocked torrent with lost mark-downloading write left variant %q", got)
	}
}

func TestVariantImportFailedKeepsRetryableClaimButExhaustionGoesToReview(t *testing.T) {
	variants, repo, book, _, dl := downloadingImportFixture(t)
	ctx := context.Background()
	if err := repo.SetErrorWithStatus(ctx, dl.ID, models.StateImportFailed, "temporary read error"); err != nil {
		t.Fatal(err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantDownloading {
		t.Fatalf("active import retry no longer holds the variant: %q", got)
	}
	for i := 0; i < 5; i++ {
		if err := repo.IncrementImportRetryCount(ctx, dl.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.SetErrorWithStatus(ctx, dl.ID, models.StateImportFailed, "staging database unavailable"); err != nil {
		t.Fatal(err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantReview {
		t.Fatalf("exhausted importFailed still leaves variant %q", got)
	}
	if err := repo.SetErrorWithStatus(ctx, dl.ID, models.StateImportBlocked, "retry limit reached"); err != nil {
		t.Fatal(err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantReview {
		t.Fatalf("blocking exhausted retry cleared review: %q", got)
	}
}

func TestVariantTerminalImportKeepsManualReviewAndImportedFile(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"manual review", models.VariantReview},
		{"already imported sibling", models.VariantImported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			variants, repo, book, variant, dl := downloadingImportFixture(t)
			ctx := context.Background()
			if tc.name == "manual review" {
				if err := variants.SetStatus(ctx, variant.ID, models.VariantReview); err != nil {
					t.Fatal(err)
				}
			} else {
				// Simulate a crash after a sibling's file was tracked, before the
				// variant status reached imported. A blocked download cannot undo it.
				if _, err := repo.db.ExecContext(ctx, `INSERT INTO book_files
					(book_id, format, path, variant_id, language, verification)
					VALUES (?, 'ebook', '/lib/already-imported.epub', ?, 'eng', 'verified')`, book.ID, variant.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := repo.SetErrorWithStatus(ctx, dl.ID, models.StateImportBlocked, "staging failed"); err != nil {
				t.Fatal(err)
			}
			if got := terminalVariantStatus(t, variants, book.ID); got != tc.want {
				t.Fatalf("variant after blocked sibling = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVariantTerminalImportKeepsOtherActiveDownload(t *testing.T) {
	variants, repo, book, variant, dl := downloadingImportFixture(t)
	ctx := context.Background()
	// Historical/manual rows may overlap even though new grabs are serialized.
	// One still-active sibling must continue to hold the variant.
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO downloads (guid, book_id, variant_id, title, nzb_url, status, protocol)
		VALUES ('second-attempt', ?, ?, 'second attempt', '', 'downloading', 'torrent')`, book.ID, variant.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetErrorWithStatus(ctx, dl.ID, models.StateImportBlocked, "first import blocked"); err != nil {
		t.Fatal(err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantDownloading {
		t.Fatalf("other active download did not hold variant: %q", got)
	}
}

func TestVariantTerminalImportRollsBackDownloadOnReconcileError(t *testing.T) {
	variants, repo, book, _, dl := downloadingImportFixture(t)
	ctx := context.Background()
	if _, err := repo.db.ExecContext(ctx, `CREATE TRIGGER reject_terminal_review BEFORE UPDATE OF status ON book_variants
		BEGIN SELECT RAISE(ABORT, 'reconcile rejected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetErrorWithStatus(ctx, dl.ID, models.StateImportBlocked, "destination collision"); err == nil {
		t.Fatal("terminal download transition succeeded without its variant reconciliation")
	}
	row, err := repo.GetByID(ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateImporting || row.ErrorMessage != "" {
		t.Fatalf("failed reconciliation partially committed download: %+v %v", row, err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantDownloading {
		t.Fatalf("failed reconciliation changed variant: %q", got)
	}
}

func TestLegacyImportBlockedWithoutVariantKeepsDownloadPath(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	repo := NewDownloadRepo(database)
	dl := &models.Download{GUID: "legacy-import", BookID: &book.ID, Title: "Legacy", Status: models.StateImporting, Protocol: "torrent"}
	if err := repo.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetImportPath(ctx, dl.ID, "/seed/legacy.epub"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetErrorWithStatus(ctx, dl.ID, models.StateImportBlocked, "old importer failure"); err != nil {
		t.Fatal(err)
	}
	row, err := repo.GetByID(ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateImportBlocked || row.ImportPath != "/seed/legacy.epub" {
		t.Fatalf("legacy blocked download changed: %+v %v", row, err)
	}
}
