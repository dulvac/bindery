package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// A crashed fifth import attempt may be the last event before the torrent
// client becomes unreachable. Recovery must close its claim without polling.
func TestVariantInterruptedImportExhaustedReconcilesWithUnavailableClient(t *testing.T) {
	variants, repo, book, _, dl := downloadingImportFixture(t)
	ctx := context.Background()
	client := &models.DownloadClient{Name: "unreachable", Type: "qbittorrent", Host: "127.0.0.1", Port: 1}
	if err := NewDownloadClientRepo(repo.db).Create(ctx, client); err != nil {
		t.Fatal(err)
	}
	const hash = "abcdef1234567890abcdef1234567890abcdef12"
	if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET download_client_id=?, torrent_id=?, import_retry_count=? WHERE id=?`, client.ID, hash, ImportRetryLimit, dl.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := repo.RecoverInterruptedImports(ctx)
	if err != nil || len(ids) != 1 || ids[0] != dl.ID {
		t.Fatalf("recovery = %v, %v; want old torrent recovered", ids, err)
	}
	row, err := repo.GetByID(ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateImportFailed || row.ImportRetryCount != ImportRetryLimit || row.TorrentID == nil || *row.TorrentID != hash || row.DownloadClientID == nil || *row.DownloadClientID != client.ID {
		t.Fatalf("old torrent changed beyond the recovery status: %+v %v", row, err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantReview {
		t.Fatalf("exhausted interrupted import left variant %q; want review without client polling", got)
	}
	if wanted, err := variants.ListWanted(ctx); err != nil || len(wanted) != 0 {
		t.Fatalf("old torrent available for an unattended new grab: %+v %v", wanted, err)
	}
}

func TestVariantInterruptedImportRetryableKeepsDownloading(t *testing.T) {
	variants, repo, book, _, dl := downloadingImportFixture(t)
	ctx := context.Background()
	if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET import_retry_count=? WHERE id=?`, ImportRetryLimit-1, dl.ID); err != nil {
		t.Fatal(err)
	}
	if ids, err := repo.RecoverInterruptedImports(ctx); err != nil || len(ids) != 1 {
		t.Fatalf("recover retryable import: %v %v", ids, err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantDownloading {
		t.Fatalf("retryable interrupted import lost claim: %q", got)
	}
}

func TestVariantInterruptedImportExhaustedRespectsOtherClaimAndReview(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"other active download", models.VariantDownloading},
		{"imported sibling file", models.VariantImported},
		{"manual review", models.VariantReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			variants, repo, book, variant, dl := downloadingImportFixture(t)
			ctx := context.Background()
			if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET import_retry_count=? WHERE id=?`, ImportRetryLimit, dl.ID); err != nil {
				t.Fatal(err)
			}
			switch tc.name {
			case "other active download":
				if _, err := repo.db.ExecContext(ctx, `INSERT INTO downloads (guid, book_id, variant_id, title, nzb_url, status, protocol)
					VALUES ('other-active-recovery', ?, ?, 'other', '', 'downloading', 'torrent')`, book.ID, variant.ID); err != nil {
					t.Fatal(err)
				}
			case "imported sibling file":
				if _, err := repo.db.ExecContext(ctx, `INSERT INTO book_files (book_id, format, path, variant_id, language, verification)
					VALUES (?, 'ebook', '/lib/recovered-sibling.epub', ?, 'eng', 'verified')`, book.ID, variant.ID); err != nil {
					t.Fatal(err)
				}
			case "manual review":
				if err := variants.SetStatus(ctx, variant.ID, models.VariantReview); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := repo.RecoverInterruptedImports(ctx); err != nil {
				t.Fatal(err)
			}
			if got := terminalVariantStatus(t, variants, book.ID); got != tc.want {
				t.Fatalf("recovery changed variant to %q; want %q", got, tc.want)
			}
		})
	}
}

// Another in-flight download may keep the variant downloading when the old
// import blocks. The generic queue action still must not rearm that old torrent.
func TestVariantBlockedTorrentWithOtherActiveRejectsGenericRetry(t *testing.T) {
	variants, repo, book, variant, old := downloadingImportFixture(t)
	ctx := context.Background()
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO downloads (guid, book_id, variant_id, title, nzb_url, status, protocol)
		VALUES ('other-active-while-blocked', ?, ?, 'other', '', 'downloading', 'torrent')`, book.ID, variant.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetErrorWithStatus(ctx, old.ID, models.StateImportBlocked, "cannot place file"); err != nil {
		t.Fatal(err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantDownloading {
		t.Fatalf("other download lost claim: %q", got)
	}
	accepted, found, err := repo.ResetImportRetry(ctx, old.ID)
	if err != nil || accepted || !found {
		t.Fatalf("unnamed retry rearmed old blocked torrent alongside another active one: %t %t %v", accepted, found, err)
	}
	row, err := repo.GetByID(ctx, old.ID)
	if err != nil || row == nil || row.Status != models.StateImportBlocked {
		t.Fatalf("generic retry changed blocked torrent: %+v %v", row, err)
	}
}

// A second active download keeps the variant downloading until it finishes.
// If it fails, the exhausted old torrent must send the variant to review,
// not reopen an unattended new grab.
func TestVariantExhaustedRecoveryOtherDownloadFailureNeedsReview(t *testing.T) {
	variants, repo, book, variant, old := downloadingImportFixture(t)
	ctx := context.Background()
	result, err := repo.db.ExecContext(ctx, `INSERT INTO downloads (guid, book_id, variant_id, title, nzb_url, status, protocol)
		VALUES ('other-active-after-recovery', ?, ?, 'other', '', 'downloading', 'torrent')`, book.ID, variant.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET import_retry_count=? WHERE id=?`, ImportRetryLimit, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RecoverInterruptedImports(ctx); err != nil {
		t.Fatal(err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantDownloading {
		t.Fatalf("other active download did not hold variant: %q", got)
	}
	if err := repo.UpdateStatus(ctx, otherID, models.StateFailed); err != nil {
		t.Fatal(err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantReview {
		t.Fatalf("failed last active download reopened unreviewed old torrent: %q", got)
	}
	if wanted, err := variants.ListWanted(ctx); err != nil || len(wanted) != 0 {
		t.Fatalf("unreviewed old torrent queued a new grab: %+v %v", wanted, err)
	}
}

// Seeking a different release is a separate, named review decision. A fifth
// crashed attempt is terminal; it must not reserve the variant forever after
// an admin explicitly chooses this path. No client send is performed here.
func TestVariantExhaustedRecoveryAllowsSeparateReviewedNewRelease(t *testing.T) {
	variants, repo, book, variant, old := downloadingImportFixture(t)
	ctx := context.Background()
	if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET import_retry_count=? WHERE id=?`, ImportRetryLimit, old.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetTorrentID(ctx, old.ID, "retained-old-hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RecoverInterruptedImports(ctx); err != nil {
		t.Fatal(err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantReview {
		t.Fatalf("old torrent needs manual review: %q", got)
	}
	approved, err := variants.RetryReviewed(ctx, book.ID, variant.ID, 42, "seek a different release")
	if err != nil || !approved {
		t.Fatalf("exhausted old import prevented a separate new-release decision: %t %v", approved, err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantWanted {
		t.Fatalf("named new-release decision left variant %q", got)
	}
	fresh := &models.Download{GUID: "separate-new-release", BookID: &book.ID, VariantID: &variant.ID,
		Title: "Other release", Protocol: "torrent", Status: models.StateGrabbed}
	if err := repo.Create(ctx, fresh); err != nil {
		t.Fatalf("exhausted old import blocked a new claim: %v", err)
	}
	row, err := repo.GetByID(ctx, old.ID)
	if err != nil || row == nil || row.Status != models.StateImportBlocked || row.ImportRetryCount != ImportRetryLimit ||
		row.TorrentID == nil || *row.TorrentID != "retained-old-hash" || row.ErrorMessage == "" {
		t.Fatalf("new release decision lost the parked old torrent: %+v %v", row, err)
	}
}

// After a new-release decision, an exhausted old import must stop being
// polled. A stale poll already in progress must not reclaim the wanted copy.
func TestVariantNewReleaseDecisionMakesExhaustedTorrentInert(t *testing.T) {
	variants, repo, book, variant, old := downloadingImportFixture(t)
	ctx := context.Background()
	const hash = "retained-old-hash"
	client := &models.DownloadClient{Name: "retained", Type: "qbittorrent", Host: "127.0.0.1", Port: 1}
	if err := NewDownloadClientRepo(repo.db).Create(ctx, client); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetTorrentID(ctx, old.ID, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET import_retry_count=?, download_client_id=? WHERE id=?`, ImportRetryLimit, client.ID, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RecoverInterruptedImports(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := repo.GetByID(ctx, old.ID)
	if err != nil || before == nil || before.Status != models.StateImportFailed || before.ErrorMessage == "" {
		t.Fatalf("expected exhausted recovered import: %+v %v", before, err)
	}
	approved, err := variants.RetryReviewed(ctx, book.ID, variant.ID, 42, "seek another release")
	if err != nil || !approved {
		t.Fatalf("approve new release: %t %v", approved, err)
	}
	row, err := repo.GetByID(ctx, old.ID)
	if err != nil || row == nil || row.Status != models.StateImportBlocked || row.ImportRetryCount != ImportRetryLimit ||
		row.TorrentID == nil || *row.TorrentID != hash || row.ErrorMessage != before.ErrorMessage || row.DeadAt == nil {
		t.Fatalf("new release did not park exhausted old torrent with its reason and identity: %+v %v", row, err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantWanted {
		t.Fatalf("new release decision lost before polling: %q", got)
	}
	var invalid models.ErrInvalidTransition
	if err := repo.SetErrorWithStatus(ctx, old.ID, models.StateImportBlocked, "stale poll tried to block again"); !errors.As(err, &invalid) {
		t.Fatalf("stale poll rewrote already blocked torrent: %v", err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantWanted {
		t.Fatalf("stale poll undid new release decision: %q", got)
	}
	if row, err = repo.GetByID(ctx, old.ID); err != nil || row == nil || row.ErrorMessage != before.ErrorMessage {
		t.Fatalf("stale poll rewrote the original failure: %+v %v", row, err)
	}
	if accepted, found, err := repo.ResetImportRetry(ctx, old.ID); err != nil || accepted || !found {
		t.Fatalf("generic retry bypassed the named action: %t %t %v", accepted, found, err)
	}
	// If this copy is later reviewed again, the old torrent remains eligible
	// for the named existing-torrent action (provided no source was rejected).
	if err := variants.SetStatus(ctx, variant.ID, models.VariantReview); err != nil {
		t.Fatal(err)
	}
	if approved, err := repo.RetryReviewedImport(ctx, book.ID, variant.ID, old.ID, 42, "retry the retained torrent"); err != nil || !approved {
		t.Fatalf("named existing-torrent retry after re-review: %t %v", approved, err)
	}
	if row, err = repo.GetByID(ctx, old.ID); err != nil || row == nil || row.Status != models.StateImportFailed ||
		row.ImportRetryCount != 0 || row.TorrentID == nil || *row.TorrentID != hash {
		t.Fatalf("named retry lost the old torrent: %+v %v", row, err)
	}
}

func TestVariantNewReleaseDecisionAuditFailureLeavesExhaustedTorrentUnchanged(t *testing.T) {
	variants, repo, book, variant, old := downloadingImportFixture(t)
	ctx := context.Background()
	if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET import_retry_count=? WHERE id=?`, ImportRetryLimit, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RecoverInterruptedImports(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `CREATE TRIGGER reject_review_retry_audit BEFORE INSERT ON history
		WHEN NEW.event_type='variantReviewRetried' BEGIN SELECT RAISE(ABORT, 'no audit'); END`); err != nil {
		t.Fatal(err)
	}
	if ok, err := variants.RetryReviewed(ctx, book.ID, variant.ID, 42, "seek a new release"); err == nil || ok {
		t.Fatalf("audit failed but new-release decision committed: %t %v", ok, err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantReview {
		t.Fatalf("failed audit changed reviewed copy: %q", got)
	}
	row, err := repo.GetByID(ctx, old.ID)
	if err != nil || row == nil || row.Status != models.StateImportFailed || row.ImportRetryCount != ImportRetryLimit {
		t.Fatalf("failed audit changed exhausted download: %+v %v", row, err)
	}
}

// Generic queue retry has no named review decision. It cannot be the second
// half of review -> wanted for the old torrent or bypass review on its own.
func TestVariantReviewedTorrentRejectsGenericImportRetry(t *testing.T) {
	for _, status := range []string{models.VariantReview, models.VariantWanted} {
		t.Run(status, func(t *testing.T) {
			variants, repo, book, variant, dl := downloadingImportFixture(t)
			ctx := context.Background()
			if err := repo.SetErrorWithStatus(ctx, dl.ID, models.StateImportBlocked, "cannot place file"); err != nil {
				t.Fatal(err)
			}
			if status == models.VariantWanted {
				if ok, err := variants.RetryReviewed(ctx, book.ID, variant.ID, 42, "seek another release"); err != nil || !ok {
					t.Fatalf("seek new release: %t %v", ok, err)
				}
			}
			if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET protocol='torrent', torrent_id='existing-hash', import_retry_count=5 WHERE id=?`, dl.ID); err != nil {
				t.Fatal(err)
			}
			accepted, found, err := repo.ResetImportRetry(ctx, dl.ID)
			if err != nil || accepted || !found {
				t.Fatalf("generic retry bypassed named review: accepted=%t found=%t err=%v", accepted, found, err)
			}
			row, err := repo.GetByID(ctx, dl.ID)
			if err != nil || row == nil || row.Status != models.StateImportBlocked || row.ImportRetryCount != 5 || row.TorrentID == nil {
				t.Fatalf("generic retry changed failed torrent: %+v %v", row, err)
			}
			if got := terminalVariantStatus(t, variants, book.ID); got != status {
				t.Fatalf("generic retry changed variant to %q; want %q", got, status)
			}
		})
	}
}

func TestVariantInterruptedImportReconciliationFailureRollsBackDownload(t *testing.T) {
	variants, repo, book, _, dl := downloadingImportFixture(t)
	ctx := context.Background()
	if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET import_retry_count=? WHERE id=?`, ImportRetryLimit, dl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `CREATE TRIGGER reject_interrupted_review BEFORE UPDATE OF status ON book_variants
		BEGIN SELECT RAISE(ABORT, 'reconcile rejected'); END`); err != nil {
		t.Fatal(err)
	}
	ids, err := repo.RecoverInterruptedImports(ctx)
	if err == nil || !strings.Contains(err.Error(), "reconcile rejected") || len(ids) != 0 {
		t.Fatalf("failed reconciliation reported successful recovery: ids=%v err=%v", ids, err)
	}
	row, err := repo.GetByID(ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateImporting || row.ErrorMessage != "" {
		t.Fatalf("recovery committed download without variant: %+v %v", row, err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantDownloading {
		t.Fatalf("recovery changed variant despite failure: %q", got)
	}
}
