package importer

import (
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

// Startup has no access to the old torrent's file list when the client is
// offline. The fifth interrupted attempt still needs an actionable state and
// accurate history before any client poll can run.
func TestVariantStartupRecoveryExhaustedWithUnavailableClient(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	client := &models.DownloadClient{Name: "unreachable", Type: "qbittorrent", Host: "127.0.0.1", Port: 1}
	if err := db.NewDownloadClientRepo(f.database).Create(f.ctx, client); err != nil {
		t.Fatal(err)
	}
	hash := "abcdef1234567890abcdef1234567890abcdef12"
	dl := &models.Download{GUID: "crashed-import", Title: "English catalogue label [ENG / EPUB]", BookID: &f.book.ID,
		VariantID: &f.variant.ID, DownloadClientID: &client.ID, TorrentID: &hash, Status: models.StateGrabbed, Protocol: "torrent"}
	if err := f.downloads.Create(f.ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := f.downloads.UpdateStatus(f.ctx, dl.ID, models.StateDownloading); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.books.VariantRepo().MarkDownloading(f.ctx, f.variant.ID); err != nil || !ok {
		t.Fatalf("mark old torrent downloading: %t %v", ok, err)
	}
	for _, next := range []models.DownloadState{models.StateCompleted, models.StateImportPending, models.StateImporting} {
		if err := f.downloads.UpdateStatus(f.ctx, dl.ID, next); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < db.ImportRetryLimit; i++ {
		if err := f.downloads.IncrementImportRetryCount(f.ctx, dl.ID); err != nil {
			t.Fatal(err)
		}
	}
	f.scanner.RecoverInterruptedImports(f.ctx)
	row, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateImportFailed || row.TorrentID == nil || *row.TorrentID != hash || !strings.Contains(row.ErrorMessage, "review required") {
		t.Fatalf("recovered download not actionable with old torrent intact: %+v %v", row, err)
	}
	if got := twoKindleVariantStatus(t, f); got != models.VariantReview {
		t.Fatalf("startup with offline client left variant %q; want review", got)
	}
	events, err := f.history.ListByBook(f.ctx, f.book.ID)
	if err != nil || len(events) != 1 || events[0].EventType != models.HistoryEventImportFailed || !strings.Contains(events[0].Data, "review required") {
		t.Fatalf("startup recovery claimed a retry rather than review: %+v %v", events, err)
	}
	f.scanner.RecoverInterruptedImports(f.ctx)
	again, err := f.history.ListByBook(f.ctx, f.book.ID)
	if err != nil || len(again) != 1 {
		t.Fatalf("second startup duplicated history: %+v %v", again, err)
	}
}
