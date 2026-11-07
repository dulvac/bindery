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

// SetErrorWithStatus has a different transition table from SetError: a
// blocked row may ordinarily go back to importFailed for a legacy manual
// retry. An old importer snapshot cannot use that edge to undo the owner's
// decision to seek a new release for a variant-bound torrent.
func TestSetErrorWithStatusStaleImportFailedCannotUndoNewRelease(t *testing.T) {
	variants, repo, book, variant, dl := downloadingImportFixture(t)
	ctx := context.Background()
	client := &models.DownloadClient{Name: "retained torrent", Type: "qbittorrent", Host: "127.0.0.1", Port: 1}
	if err := NewDownloadClientRepo(repo.db).Create(ctx, client); err != nil {
		t.Fatal(err)
	}
	const hash = "abcdef1234567890abcdef1234567890abcdef12"
	if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET torrent_id=?, download_client_id=?, import_retry_count=? WHERE id=?`,
		hash, client.ID, ImportRetryLimit, dl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RecoverInterruptedImports(ctx); err != nil {
		t.Fatal(err)
	}
	old, err := repo.GetByID(ctx, dl.ID) // importer snapshot before admin action
	if err != nil || old == nil || old.Status != models.StateImportFailed || old.ErrorMessage == "" {
		t.Fatalf("old import snapshot: %+v %v", old, err)
	}
	if ok, err := variants.RetryReviewed(ctx, book.ID, variant.ID, 42, "seek another release"); err != nil || !ok {
		t.Fatalf("named new-release decision: %t %v", ok, err)
	}
	parked, err := repo.GetByID(ctx, dl.ID)
	if err != nil || parked == nil || parked.Status != models.StateImportBlocked || parked.DeadAt == nil {
		t.Fatalf("old torrent was not parked: %+v %v", parked, err)
	}
	var invalid models.ErrInvalidTransition
	if err := repo.SetErrorWithStatus(ctx, old.ID, models.StateImportFailed, "stale importer failed after review"); !errors.As(err, &invalid) || invalid.From != models.StateImportBlocked || invalid.To != models.StateImportFailed {
		t.Fatalf("stale import failure accepted: %v", err)
	}
	row, err := repo.GetByID(ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateImportBlocked || row.ErrorMessage != old.ErrorMessage ||
		row.DeadAt == nil || !row.DeadAt.Equal(*parked.DeadAt) || row.ImportRetryCount != ImportRetryLimit ||
		row.TorrentID == nil || *row.TorrentID != hash || row.DownloadClientID == nil || *row.DownloadClientID != client.ID {
		t.Fatalf("stale failure rewrote parked old torrent: %+v %v", row, err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantWanted {
		t.Fatalf("stale importer undid new-release decision: %q", got)
	}
	wanted, err := variants.ListWanted(ctx)
	if err != nil || len(wanted) != 1 || wanted[0].ID != variant.ID {
		t.Fatalf("new release request lost without a search: %+v %v", wanted, err)
	}
	events, err := NewHistoryRepo(repo.db).ListByBook(ctx, book.ID)
	if err != nil || len(events) != 1 || events[0].EventType != "variantReviewRetried" {
		t.Fatalf("stale failure wrote history beyond named decision: %+v %v", events, err)
	}
	if err := variants.SetStatus(ctx, variant.ID, models.VariantReview); err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.RetryReviewedImport(ctx, book.ID, variant.ID, dl.ID, 42, "retry retained torrent"); err != nil || !ok {
		t.Fatalf("named existing-torrent retry was blocked: %t %v", ok, err)
	}
	row, err = repo.GetByID(ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateImportFailed || row.ImportRetryCount != 0 ||
		row.TorrentID == nil || *row.TorrentID != hash {
		t.Fatalf("named existing-torrent retry lost identity: %+v %v", row, err)
	}
}

// A real active import still fails normally, but failure of its terminal
// variant reconciliation must leave both rows and the prior reason intact.
func TestSetErrorWithStatusImportFailedReconcileErrorRollsBackBothRows(t *testing.T) {
	variants, repo, book, _, dl := downloadingImportFixture(t)
	ctx := context.Background()
	if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET import_retry_count=?, error_message=? WHERE id=?`,
		ImportRetryLimit, "original import reason", dl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `CREATE TRIGGER reject_failed_import_review BEFORE UPDATE OF status ON book_variants
		BEGIN SELECT RAISE(ABORT, 'reconcile rejected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetErrorWithStatus(ctx, dl.ID, models.StateImportFailed, "new import failure"); err == nil || !strings.Contains(err.Error(), "reconcile rejected") {
		t.Fatalf("failed reconciliation was accepted: %v", err)
	}
	row, err := repo.GetByID(ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateImporting || row.ErrorMessage != "original import reason" ||
		row.DeadAt != nil || row.ImportRetryCount != ImportRetryLimit {
		t.Fatalf("DB failure partially changed download: %+v %v", row, err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantDownloading {
		t.Fatalf("DB failure partially changed variant: %q", got)
	}
}

func TestSetErrorWithStatusLegacyBlockedToImportFailedStillAllowed(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	repo := NewDownloadRepo(database)
	dl := &models.Download{GUID: "legacy-blocked-retry", BookID: &book.ID, Title: "Legacy torrent",
		Protocol: "torrent", Status: models.StateImporting}
	if err := repo.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetTorrentID(ctx, dl.ID, "legacy-hash"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetErrorWithStatus(ctx, dl.ID, models.StateImportBlocked, "legacy failure"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetErrorWithStatus(ctx, dl.ID, models.StateImportFailed, "legacy manual retry"); err != nil {
		t.Fatalf("legacy blocked-to-failed transition: %v", err)
	}
	row, err := repo.GetByID(ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateImportFailed || row.ErrorMessage != "legacy manual retry" ||
		row.DeadAt != nil || row.VariantID != nil || row.TorrentID == nil || *row.TorrentID != "legacy-hash" {
		t.Fatalf("legacy no-variant transition changed: %+v %v", row, err)
	}
}

// A generic import transition must not rearm a blocked variant torrent,
// whether the variant is still in review or an admin has requested a new
// release. Only the named old-torrent retry may rearm that import.
func TestUpdateStatusVariantBlockedCannotGenericImportPending(t *testing.T) {
	for _, seekNew := range []bool{false, true} {
		name := "still in review"
		if seekNew {
			name = "new release requested"
		}
		t.Run(name, func(t *testing.T) {
			variants, repo, book, variant, dl := reviewedTorrentImportFixture(t)
			ctx := context.Background()
			if seekNew {
				if ok, err := variants.RetryReviewed(ctx, book.ID, variant.ID, 42, "seek another release"); err != nil || !ok {
					t.Fatalf("seek new release: %t %v", ok, err)
				}
			}
			parked, err := repo.GetByID(ctx, dl.ID)
			if err != nil || parked == nil || parked.DeadAt == nil || parked.TorrentID == nil {
				t.Fatalf("parked torrent: %+v %v", parked, err)
			}
			var invalid models.ErrInvalidTransition
			if err := repo.UpdateStatus(ctx, dl.ID, models.StateImportPending); !errors.As(err, &invalid) || invalid.From != models.StateImportBlocked || invalid.To != models.StateImportPending {
				t.Fatalf("generic retry of variant-bound blocked torrent = %v", err)
			}
			row, err := repo.GetByID(ctx, dl.ID)
			if err != nil || row == nil || row.Status != models.StateImportBlocked || row.ErrorMessage != parked.ErrorMessage ||
				row.ImportRetryCount != parked.ImportRetryCount || row.DeadAt == nil || !row.DeadAt.Equal(*parked.DeadAt) ||
				row.TorrentID == nil || *row.TorrentID != *parked.TorrentID {
				t.Fatalf("generic retry changed parked torrent: %+v %v", row, err)
			}
			want := models.VariantReview
			if seekNew {
				want = models.VariantWanted
			}
			if got := terminalVariantStatus(t, variants, book.ID); got != want {
				t.Fatalf("generic retry changed variant to %q; want %q", got, want)
			}
			if seekNew {
				if err := variants.SetStatus(ctx, variant.ID, models.VariantReview); err != nil {
					t.Fatal(err)
				}
			}
			if ok, err := repo.RetryReviewedImport(ctx, book.ID, variant.ID, dl.ID, 42, "retry retained torrent"); err != nil || !ok {
				t.Fatalf("named existing-torrent retry refused: %t %v", ok, err)
			}
			row, err = repo.GetByID(ctx, dl.ID)
			if err != nil || row == nil || row.Status != models.StateImportFailed || row.ImportRetryCount != 0 || row.DeadAt != nil ||
				row.TorrentID == nil || *row.TorrentID != *parked.TorrentID || terminalVariantStatus(t, variants, book.ID) != models.VariantDownloading {
				t.Fatalf("named retry lost retained torrent or variant claim: %+v %v", row, err)
			}
			if err := repo.UpdateStatus(ctx, dl.ID, models.StateImportPending); err != nil {
				t.Fatalf("import cannot proceed after named retry: %v", err)
			}
		})
	}
}

// A legacy blocked download has no variant decision to undo. Its direct
// manual import recovery still moves into the import flow and clears dead_at.
func TestUpdateStatusLegacyBlockedCanManualImportPending(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	repo := NewDownloadRepo(database)
	dl := &models.Download{GUID: "legacy-manual-retry", BookID: &book.ID, Title: "Legacy download",
		Protocol: "torrent", Status: models.StateImporting}
	if err := repo.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetTorrentID(ctx, dl.ID, "legacy-seeding-hash"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetErrorWithStatus(ctx, dl.ID, models.StateImportBlocked, "legacy import needs repair"); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateStatus(ctx, dl.ID, models.StateImportPending); err != nil {
		t.Fatalf("legacy manual import recovery refused: %v", err)
	}
	row, err := repo.GetByID(ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateImportPending || row.DeadAt != nil ||
		row.ErrorMessage != "legacy import needs repair" || row.VariantID != nil ||
		row.TorrentID == nil || *row.TorrentID != "legacy-seeding-hash" {
		t.Fatalf("legacy recovery changed download identity: %+v %v", row, err)
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
