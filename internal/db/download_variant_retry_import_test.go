package db

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

func reviewedTorrentImportFixture(t *testing.T) (*BookVariantRepo, *DownloadRepo, *models.Book, *models.BookVariant, *models.Download) {
	t.Helper()
	variants, repo, book, variant, dl := downloadingImportFixture(t)
	ctx := context.Background()
	client := &models.DownloadClient{Name: "existing-client", Type: "qbittorrent", Host: "127.0.0.1", Port: 1}
	if err := NewDownloadClientRepo(repo.db).Create(ctx, client); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET download_client_id=? WHERE id=?`, client.ID, dl.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetTorrentID(ctx, dl.ID, "abcdef1234567890abcdef1234567890abcdef12"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetErrorWithStatus(ctx, dl.ID, models.StateImportBlocked, "file list unavailable"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET import_retry_count=5 WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantReview {
		t.Fatalf("fixture did not reach review: %q", got)
	}
	return variants, repo, book, variant, dl
}

// A named decision to retry the same torrent cannot expose wanted between
// download re-arming and variant claiming. The old torrent ID must survive.
func TestRetryReviewedImportAtomicallyRearmsExistingTorrent(t *testing.T) {
	variants, repo, book, variant, dl := reviewedTorrentImportFixture(t)
	ctx := context.Background()
	before, err := repo.GetByID(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := repo.RetryReviewedImport(ctx, book.ID, variant.ID, dl.ID, 42, "qBittorrent Files list recovered")
	if err != nil || !ok {
		t.Fatalf("named existing-torrent retry: %t %v", ok, err)
	}
	row, err := repo.GetByID(ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateImportFailed || row.ImportRetryCount != 0 || row.ErrorMessage != "" || row.DeadAt != nil || row.TorrentID == nil || *row.TorrentID != *before.TorrentID || row.DownloadClientID == nil || *row.DownloadClientID != *before.DownloadClientID {
		t.Fatalf("old torrent not rearmed in place: %+v %v", row, err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantDownloading {
		t.Fatalf("review retry left variant %q; want downloading", got)
	}
	if wanted, err := variants.ListWanted(ctx); err != nil || len(wanted) != 0 {
		t.Fatalf("retry exposed wanted to scheduler: %+v %v", wanted, err)
	}
	events, err := NewHistoryRepo(repo.db).ListByBook(ctx, book.ID)
	if err != nil || len(events) != 1 || events[0].EventType != "variantImportRetried" {
		t.Fatalf("named retry audit: %+v %v", events, err)
	}
	var audit struct {
		ActorUserID int64  `json:"actorUserId"`
		VariantID   int64  `json:"variantId"`
		DownloadID  int64  `json:"downloadId"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(events[0].Data), &audit); err != nil || audit.ActorUserID != 42 || audit.VariantID != variant.ID || audit.DownloadID != dl.ID || audit.Reason != "qBittorrent Files list recovered" {
		t.Fatalf("wrong named retry audit: %+v %v", audit, err)
	}
	if again, err := repo.RetryReviewedImport(ctx, book.ID, variant.ID, dl.ID, 42, "repeat"); err != nil || again {
		t.Fatalf("duplicate retry changed state: %t %v", again, err)
	}
}

// The crash path ends in exhausted importFailed, not importBlocked. Its old
// torrent must be retryable by the same named action after a client outage.
func TestRetryReviewedImportRearmsExhaustedStartupRecovery(t *testing.T) {
	variants, repo, book, variant, dl := downloadingImportFixture(t)
	ctx := context.Background()
	client := &models.DownloadClient{Name: "offline-on-restart", Type: "qbittorrent", Host: "127.0.0.1", Port: 1}
	if err := NewDownloadClientRepo(repo.db).Create(ctx, client); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE downloads SET download_client_id=?, torrent_id=?, import_retry_count=? WHERE id=?`,
		client.ID, "retained-recovery-hash", ImportRetryLimit, dl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RecoverInterruptedImports(ctx); err != nil {
		t.Fatal(err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantReview {
		t.Fatalf("startup fixture did not require review: %q", got)
	}
	ok, err := repo.RetryReviewedImport(ctx, book.ID, variant.ID, dl.ID, 42, "old torrent client restored")
	if err != nil || !ok {
		t.Fatalf("retry recovered fifth attempt: %t %v", ok, err)
	}
	row, err := repo.GetByID(ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateImportFailed || row.ImportRetryCount != 0 || row.TorrentID == nil || *row.TorrentID != "retained-recovery-hash" || row.ErrorMessage != "" {
		t.Fatalf("old recovered download was not rearmed: %+v %v", row, err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantDownloading {
		t.Fatalf("retry left recovered variant %q", got)
	}
}

func TestRetryReviewedImportRefusesUnsafeTargetWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		change                  func(*testing.T, *DownloadRepo, *models.Book, *models.BookVariant, *models.Download)
		book, variant, download int64
	}{
		{"wrong book", nil, 100000, 0, 0},
		{"wrong variant", nil, 0, 100000, 0},
		{"wrong download", nil, 0, 0, 100000},
		{"another active download", func(t *testing.T, repo *DownloadRepo, book *models.Book, variant *models.BookVariant, dl *models.Download) {
			if _, err := repo.db.ExecContext(context.Background(), `INSERT INTO downloads (guid, book_id, variant_id, title, nzb_url, status, protocol)
				VALUES ('another-active-import', ?, ?, 'other', '', 'downloading', 'torrent')`, book.ID, variant.ID); err != nil {
				t.Fatal(err)
			}
		}, 0, 0, 0},
		{"tracked file", func(t *testing.T, repo *DownloadRepo, book *models.Book, variant *models.BookVariant, dl *models.Download) {
			if _, err := repo.db.ExecContext(context.Background(), `INSERT INTO book_files (book_id, format, path, variant_id, language, verification)
				VALUES (?, 'ebook', '/lib/already-imported.epub', ?, 'eng', 'verified')`, book.ID, variant.ID); err != nil {
				t.Fatal(err)
			}
		}, 0, 0, 0},
		{"rejected source", func(t *testing.T, repo *DownloadRepo, book *models.Book, variant *models.BookVariant, dl *models.Download) {
			if _, err := repo.db.ExecContext(context.Background(), `INSERT INTO variant_rejections (book_id, variant_id, download_id, source_path, reason)
				VALUES (?, ?, ?, '/seed/incorrect.epub', 'wrong work')`, book.ID, variant.ID, dl.ID); err != nil {
				t.Fatal(err)
			}
		}, 0, 0, 0},
		{"missing torrent identity", func(t *testing.T, repo *DownloadRepo, book *models.Book, variant *models.BookVariant, dl *models.Download) {
			if _, err := repo.db.ExecContext(context.Background(), `UPDATE downloads SET torrent_id=NULL WHERE id=?`, dl.ID); err != nil {
				t.Fatal(err)
			}
		}, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			variants, repo, book, variant, dl := reviewedTorrentImportFixture(t)
			if tc.change != nil {
				tc.change(t, repo, book, variant, dl)
			}
			bookID, variantID, downloadID := book.ID+tc.book, variant.ID+tc.variant, dl.ID+tc.download
			ok, err := repo.RetryReviewedImport(context.Background(), bookID, variantID, downloadID, 42, "try again")
			if err != nil || ok {
				t.Fatalf("unsafe retry accepted: %t %v", ok, err)
			}
			row, err := repo.GetByID(context.Background(), dl.ID)
			if err != nil || row == nil || row.Status != models.StateImportBlocked || row.ImportRetryCount != 5 || !strings.Contains(row.ErrorMessage, "file list") {
				t.Fatalf("unsafe retry altered download: %+v %v", row, err)
			}
			if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantReview {
				t.Fatalf("unsafe retry altered variant: %q", got)
			}
			events, err := NewHistoryRepo(repo.db).ListByBook(context.Background(), book.ID)
			if err != nil || len(events) != 0 {
				t.Fatalf("unsafe retry wrote audit: %+v %v", events, err)
			}
		})
	}
}

func TestRetryReviewedImportRequiresNamedActorAndReason(t *testing.T) {
	_, repo, book, variant, dl := reviewedTorrentImportFixture(t)
	for _, tc := range []struct {
		actor  int64
		reason string
	}{{0, "try again"}, {42, "  "}, {42, strings.Repeat("x", 501)}} {
		if ok, err := repo.RetryReviewedImport(context.Background(), book.ID, variant.ID, dl.ID, tc.actor, tc.reason); err == nil || ok {
			t.Fatalf("invalid named retry accepted: %t %v", ok, err)
		}
	}
}

func TestRetryReviewedImportAuditFailureRollsBackBothRows(t *testing.T) {
	variants, repo, book, variant, dl := reviewedTorrentImportFixture(t)
	ctx := context.Background()
	if _, err := repo.db.ExecContext(ctx, `CREATE TRIGGER reject_variant_import_audit BEFORE INSERT ON history
		WHEN NEW.event_type='variantImportRetried' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.RetryReviewedImport(ctx, book.ID, variant.ID, dl.ID, 42, "retry old torrent"); ok || err == nil || !strings.Contains(err.Error(), "audit unavailable") {
		t.Fatalf("audit failure allowed partial retry: %t %v", ok, err)
	}
	row, err := repo.GetByID(ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateImportBlocked || row.ImportRetryCount != 5 || row.ErrorMessage == "" {
		t.Fatalf("audit failure changed download: %+v %v", row, err)
	}
	if got := terminalVariantStatus(t, variants, book.ID); got != models.VariantReview {
		t.Fatalf("audit failure changed variant: %q", got)
	}
}
