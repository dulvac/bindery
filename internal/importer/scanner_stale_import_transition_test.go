package importer

import (
	"os"
	"testing"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

// A stale import continuation must not place a retained old torrent after a
// named new-release decision. The refused transition must stop before any
// placement, history, webhook or torrent source mutation.
func TestVariantStaleImportContinuationCannotPlaceParkedOldTorrent(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	dl, mutators := terminalQbitDownload(t, f, "English catalogue label [ENG / EPUB]", source, source, nil)
	for _, state := range []models.DownloadState{models.StateCompleted, models.StateImportPending, models.StateImporting} {
		if err := f.downloads.UpdateStatus(f.ctx, dl.ID, state); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < db.ImportRetryLimit; i++ {
		if err := f.downloads.IncrementImportRetryCount(f.ctx, dl.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.downloads.RecoverInterruptedImports(f.ctx); err != nil {
		t.Fatal(err)
	}
	stale, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || stale == nil || stale.Status != models.StateImportFailed {
		t.Fatalf("snapshot: %+v %v", stale, err)
	}
	if ok, err := f.books.VariantRepo().RetryReviewed(f.ctx, f.book.ID, f.variant.ID, 42, "seek another release"); err != nil || !ok {
		t.Fatalf("seek new release: %t %v", ok, err)
	}
	parked, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || parked == nil || parked.DeadAt == nil || parked.TorrentID == nil || stale.TorrentID == nil {
		t.Fatalf("old torrent not parked with its hash: %+v %v", parked, err)
	}
	spy := &spyNotifier{}
	f.scanner.notif = spy
	f.scanner.tryImportInternal(f.ctx, stale, source, "qbittorrent", *stale.TorrentID, "", nil, []string{source})
	row, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	variants, err := f.books.VariantRepo().ListByBook(f.ctx, f.book.ID)
	if err != nil {
		t.Fatal(err)
	}
	files, err := f.books.ListFiles(f.ctx, f.book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.Status != models.StateImportBlocked || row.ErrorMessage != parked.ErrorMessage ||
		row.DeadAt == nil || !row.DeadAt.Equal(*parked.DeadAt) || row.ImportRetryCount != parked.ImportRetryCount ||
		row.TorrentID == nil || *row.TorrentID != *parked.TorrentID ||
		len(variants) != 1 || variants[0].Status != models.VariantWanted || len(files) != 0 {
		t.Fatalf("stale importer revived the parked torrent: download=%+v variant=%+v library files=%d", row, variants, len(files))
	}
	f.assertLibraryFiles(t, 0)
	events, err := f.history.ListByBook(f.ctx, f.book.ID)
	if err != nil || len(events) != 1 || events[0].EventType != "variantReviewRetried" || len(spy.calls) != 0 {
		t.Fatalf("stale importer published a false event: %+v %v, webhook=%+v", events, err, spy.calls)
	}
	assertTerminalSourceUnchanged(t, source, before, mutators)
	if err := f.books.VariantRepo().SetStatus(f.ctx, f.variant.ID, models.VariantReview); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.downloads.RetryReviewedImport(f.ctx, f.book.ID, f.variant.ID, dl.ID, 42, "retry retained torrent"); err != nil || !ok {
		t.Fatalf("named existing-torrent retry no longer available: %t %v", ok, err)
	}
}
