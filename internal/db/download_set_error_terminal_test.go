package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// A client poll can hold an importFailed snapshot while the admin parks that
// torrent and explicitly asks for a different release. The stale error must
// not destroy the retained torrent's reason or its later named retry option.
func TestSetErrorStaleImportSnapshotCannotOverwriteParkedTorrent(t *testing.T) {
	variants, repo, book, variant, dl := downloadingImportFixture(t)
	ctx := context.Background()
	client := &models.DownloadClient{Name: "retained torrent", Type: "qbittorrent", Host: "127.0.0.1", Port: 1}
	if err := NewDownloadClientRepo(repo.db).Create(ctx, client); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetTorrentID(ctx, dl.ID, "retained-old-hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET import_retry_count=?, download_client_id=? WHERE id=?`, ImportRetryLimit, client.ID, dl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RecoverInterruptedImports(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.List(ctx) // poller's snapshot, taken before the named decision
	if err != nil || len(rows) != 1 || rows[0].Status != models.StateImportFailed {
		t.Fatalf("old poll snapshot = %+v, %v", rows, err)
	}
	approved, err := variants.RetryReviewed(ctx, book.ID, variant.ID, 42, "seek another release")
	if err != nil || !approved {
		t.Fatalf("park old torrent: %t %v", approved, err)
	}
	parked, err := repo.GetByID(ctx, dl.ID)
	if err != nil || parked == nil || parked.Status != models.StateImportBlocked || parked.DeadAt == nil || parked.ErrorMessage == "" {
		t.Fatalf("parked download = %+v, %v", parked, err)
	}
	// A stale poll must not even attempt the variant write: a trigger would
	// reject it if the guarded download transition incorrectly continued.
	if _, err := repo.db.ExecContext(ctx, `CREATE TRIGGER reject_stale_variant_release BEFORE UPDATE OF status ON book_variants
		BEGIN SELECT RAISE(ABORT, 'unexpected variant release'); END`); err != nil {
		t.Fatal(err)
	}
	var invalid models.ErrInvalidTransition
	if err := repo.SetError(ctx, rows[0].ID, "stale qBittorrent client error"); !errors.As(err, &invalid) {
		t.Fatalf("stale client error was accepted: %v", err)
	}
	if _, err := repo.db.ExecContext(ctx, `DROP TRIGGER reject_stale_variant_release`); err != nil {
		t.Fatal(err)
	}
	after, err := repo.GetByID(ctx, dl.ID)
	if err != nil || after == nil || after.Status != models.StateImportBlocked || after.ErrorMessage != parked.ErrorMessage ||
		after.DeadAt == nil || !after.DeadAt.Equal(*parked.DeadAt) || after.TorrentID == nil || *after.TorrentID != "retained-old-hash" ||
		after.ImportRetryCount != ImportRetryLimit {
		t.Fatalf("stale error rewrote parked torrent: %+v, %v", after, err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantWanted {
		t.Fatalf("stale error undid named new-release decision: %q", got)
	}
	// The retained torrent can still be selected for a named old-torrent
	// retry if the variant later enters review again.
	if err := variants.SetStatus(ctx, variant.ID, models.VariantReview); err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.RetryReviewedImport(ctx, book.ID, variant.ID, dl.ID, 42, "retry retained torrent"); err != nil || !ok {
		t.Fatalf("stale error broke named existing-torrent retry: %t %v", ok, err)
	}
}

// A failed variant release must roll back the already-guarded download
// transition, including its error reason and re-grab cooldown timestamp.
func TestSetErrorVariantReconcileFailureRollsBackActiveDownload(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	variants := NewBookVariantRepo(database)
	copies, err := variants.EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewDownloadRepo(database)
	dl := &models.Download{GUID: "atomic-failure", BookID: &book.ID, VariantID: &copies[0].ID,
		Title: "Sample Book [ENG / EPUB]", Protocol: "usenet", Status: models.StateGrabbed,
		ErrorMessage: "previous note"}
	if err := repo.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateStatus(ctx, dl.ID, models.StateDownloading); err != nil {
		t.Fatal(err)
	}
	if marked, err := variants.MarkDownloading(ctx, copies[0].ID); err != nil || !marked {
		t.Fatalf("mark variant downloading: %t %v", marked, err)
	}
	if _, err := database.ExecContext(ctx, `CREATE TRIGGER reject_failed_release BEFORE UPDATE OF status ON book_variants
		WHEN NEW.status = 'wanted' BEGIN SELECT RAISE(ABORT, 'reconcile rejected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetError(ctx, dl.ID, "client gave up"); err == nil || !strings.Contains(err.Error(), "reconcile rejected") {
		t.Fatalf("failed variant reconciliation = %v, want trigger error", err)
	}
	row, err := repo.GetByID(ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateDownloading || row.ErrorMessage != "previous note" || row.DeadAt != nil {
		t.Fatalf("failed reconciliation partially committed download: %+v %v", row, err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantDownloading {
		t.Fatalf("failed reconciliation changed variant: %q", got)
	}
	if _, err := database.ExecContext(ctx, `DROP TRIGGER reject_failed_release`); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetError(ctx, dl.ID, "client gave up"); err != nil {
		t.Fatalf("retry after database repair: %v", err)
	}
	row, err = repo.GetByID(ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateFailed || row.ErrorMessage != "client gave up" || row.DeadAt == nil {
		t.Fatalf("accepted failure did not persist download: %+v %v", row, err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantWanted {
		t.Fatalf("accepted failure did not release variant: %q", got)
	}
}

func TestSetErrorRefusesTerminalDownloadStates(t *testing.T) {
	for _, state := range []models.DownloadState{models.StateImportBlocked, models.StateImported, models.StateFailed} {
		t.Run(string(state), func(t *testing.T) {
			database, _, _ := openTestDB(t)
			repo := NewDownloadRepo(database)
			ctx := context.Background()
			dl := &models.Download{GUID: "terminal-error-" + string(state), Title: "finished", Protocol: "torrent", Status: state, ErrorMessage: "original reason"}
			if err := repo.Create(ctx, dl); err != nil {
				t.Fatal(err)
			}
			var invalid models.ErrInvalidTransition
			if err := repo.SetError(ctx, dl.ID, "late client error"); !errors.As(err, &invalid) {
				t.Fatalf("terminal row accepted client error: %v", err)
			}
			row, err := repo.GetByID(ctx, dl.ID)
			if err != nil || row == nil || row.Status != state || row.ErrorMessage != "original reason" {
				t.Fatalf("terminal row changed: %+v %v", row, err)
			}
		})
	}
}
