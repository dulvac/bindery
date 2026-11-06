package importer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/downloader/sabnzbd"
	"github.com/vavallee/bindery/internal/models"
)

// The qBittorrent HTTP response lands after List has snapped importFailed.
// Park the row inside the fake client's first response, before the scanner
// acts on that snapshot's client error. No sleep or live client is involved.
func TestVariantStaleQbitErrorAfterParkKeepsNamedRetryAndNoFalseEvent(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	const hash = "abcdef1234567890abcdef1234567890abcdef12"
	source := filepath.Join(f.sources, "old.epub")
	before := []byte("old torrent remains on disk")
	if err := os.WriteFile(source, before, 0o600); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	var unexpected atomic.Int32
	parkResult := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			once.Do(func() {
				ok, err := f.books.VariantRepo().RetryReviewed(f.ctx, f.book.ID, f.variant.ID, 42, "seek another release")
				if err == nil && !ok {
					err = fmt.Errorf("reviewed variant was not retryable")
				}
				parkResult <- err
			})
			_ = json.NewEncoder(w).Encode([]map[string]any{{"hash": hash, "name": "old.epub", "category": "mam", "state": "error",
				"progress": 0.5, "size": 8192, "amount_left": 4096, "save_path": f.sources, "content_path": source}})
		default:
			unexpected.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	host, port := scannerTestHostPort(t, srv.URL)
	client := &models.DownloadClient{Name: "test-qbit-stale", Type: "qbittorrent", Host: host, Port: port, Category: "mam", Enabled: true}
	if err := db.NewDownloadClientRepo(f.database).Create(f.ctx, client); err != nil {
		t.Fatal(err)
	}
	dl := &models.Download{GUID: "old-stale-qbit", Title: "Old torrent [ENG / EPUB]", Status: models.StateGrabbed, Protocol: "torrent",
		BookID: &f.book.ID, VariantID: &f.variant.ID, DownloadClientID: &client.ID}
	if err := f.downloads.Create(f.ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := f.downloads.SetTorrentID(f.ctx, dl.ID, hash); err != nil {
		t.Fatal(err)
	}
	if err := f.downloads.UpdateStatus(f.ctx, dl.ID, models.StateDownloading); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.books.VariantRepo().MarkDownloading(f.ctx, f.variant.ID); err != nil || !ok {
		t.Fatalf("initial grab not downloading: %t %v", ok, err)
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
	if _, err := f.downloads.RecoverInterruptedImports(f.ctx); err != nil {
		t.Fatal(err)
	}
	initial, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || initial == nil || initial.Status != models.StateImportFailed || initial.ErrorMessage == "" {
		t.Fatalf("old import was not ready for review: %+v %v", initial, err)
	}
	spy := &spyNotifier{}
	f.scanner.notif = spy
	f.scanner.checkQbittorrentDownloads(f.ctx, client)
	select {
	case err := <-parkResult:
		if err != nil {
			t.Fatalf("fake qBittorrent could not park download after snapshot: %v", err)
		}
	default:
		t.Fatal("fake qBittorrent was not polled after snapshot")
	}
	row, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateImportBlocked || row.ErrorMessage != initial.ErrorMessage ||
		row.TorrentID == nil || *row.TorrentID != hash || row.ImportRetryCount != db.ImportRetryLimit {
		t.Fatalf("stale qBittorrent error replaced retained import reason or retry state: %+v %v", row, err)
	}
	if !row.BlocksAutoRegrab() {
		t.Fatal("parked old GUID became eligible for an unattended re-grab")
	}
	wanted, err := f.books.VariantRepo().ListWanted(f.ctx)
	if err != nil || len(wanted) != 1 || wanted[0].ID != f.variant.ID {
		t.Fatalf("stale qBittorrent error undid named new-release decision: %+v %v", wanted, err)
	}
	events, err := f.history.ListByBook(f.ctx, f.book.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.EventType == models.HistoryEventDownloadFailed {
			t.Fatalf("false download failure history after rejected stale write: %+v", event)
		}
	}
	if spy.lookup(notifierEventDownloadFailed) != nil {
		t.Fatalf("false failure webhook after rejected stale write: %+v", spy.calls)
	}
	if data, err := os.ReadFile(source); err != nil || !bytes.Equal(data, before) || unexpected.Load() != 0 {
		t.Fatalf("fake qBittorrent mutated source or called unexpected endpoint: data=%q err=%v unexpected=%d", data, err, unexpected.Load())
	}
	if err := f.books.VariantRepo().SetStatus(f.ctx, f.variant.ID, models.VariantReview); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.downloads.RetryReviewedImport(f.ctx, f.book.ID, f.variant.ID, dl.ID, 42, "retry retained torrent"); err != nil || !ok {
		t.Fatalf("old torrent no longer eligible for named retry: %t %v", ok, err)
	}
}

// SAB shares SetError with qBittorrent. The guard must still accept a real
// active usenet failure, release its claim, and announce the successful write.
func TestVariantActiveSABErrorStillFailsAndEmitsEvent(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	const nzo = "SABnzbd_active_failure"
	const reason = "SAB transport timeout"
	srv := httptest.NewServer(sabHistoryHandler(t, []sabnzbd.HistorySlot{{NzoID: nzo, Name: "Book", Status: "Failed", FailMessage: reason}}, nil))
	t.Cleanup(srv.Close)
	client := sabClient(t, f.ctx, db.NewDownloadClientRepo(f.database), srv.URL)
	nzoID := nzo
	dl := &models.Download{GUID: "active-sab-error", Title: "Book", Status: models.StateGrabbed, Protocol: "usenet",
		BookID: &f.book.ID, VariantID: &f.variant.ID, DownloadClientID: &client.ID, SABnzbdNzoID: &nzoID}
	if err := f.downloads.Create(f.ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := f.downloads.UpdateStatus(f.ctx, dl.ID, models.StateDownloading); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.books.VariantRepo().MarkDownloading(f.ctx, f.variant.ID); err != nil || !ok {
		t.Fatalf("active SAB grab did not hold variant: %t %v", ok, err)
	}
	spy := &spyNotifier{}
	f.scanner.notif = spy
	f.scanner.checkSABnzbdDownloads(f.ctx, client)
	row, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || row == nil || row.Status != models.StateFailed || row.ErrorMessage != reason || row.DeadAt == nil {
		t.Fatalf("active SAB failure not persisted: %+v %v", row, err)
	}
	copies, err := f.books.VariantRepo().ListByBook(f.ctx, f.book.ID)
	if err != nil || len(copies) != 1 || copies[0].Status != models.VariantWanted {
		t.Fatalf("active SAB failure did not release variant: %+v %v", copies, err)
	}
	events, err := f.history.ListByBook(f.ctx, f.book.ID)
	if err != nil || len(events) != 1 || events[0].EventType != models.HistoryEventDownloadFailed || spy.lookup(notifierEventDownloadFailed) == nil {
		t.Fatalf("active SAB failure lost event or webhook: %+v %v, webhook=%+v", events, err, spy.calls)
	}
}
