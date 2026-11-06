package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/downloader"
	"github.com/vavallee/bindery/internal/models"
)

// A stall poll took a downloading snapshot before an import was blocked and
// an admin chose a different release. The old snapshot cannot fail the parked
// torrent or generate a stall event, blocklist entry, or new search.
func TestStallPollStaleSnapshotDoesNotActOnReviewedTorrent(t *testing.T) {
	s, search, downloads, book, sends := variantGrabFixture(t)
	spy := &spyNotifier{}
	s.WithNotifier(spy)
	ctx := context.Background()
	copies, err := s.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	hash := "abcdef1234567890abcdef1234567890abcdef12"
	var unexpected atomic.Int32
	qbit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			// The snapshot has already been read by checkStalledDownloads.
			dl, getErr := downloads.GetByGUID(ctx, "stale-stall")
			if getErr != nil || dl == nil {
				t.Errorf("reload download during poll: %+v %v", dl, getErr)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			for _, next := range []models.DownloadState{models.StateCompleted, models.StateImportPending, models.StateImporting} {
				if err := downloads.UpdateStatus(ctx, dl.ID, next); err != nil {
					t.Errorf("import transition to %s: %v", next, err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
			}
			if err := downloads.SetErrorWithStatus(ctx, dl.ID, models.StateImportBlocked, "file list unavailable"); err != nil {
				t.Errorf("block import: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if ok, err := s.books.VariantRepo().RetryReviewed(ctx, book.ID, copies[0].ID, 42, "seek another release"); err != nil || !ok {
				t.Errorf("review new-release decision: %t %v", ok, err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"hash": hash, "state": "stalledDL", "progress": 0.25}})
		default:
			unexpected.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(qbit.Close)
	host, port := stallServerHostPort(t, qbit.URL)
	client := &models.DownloadClient{Name: "retained-qbit", Type: "qbittorrent", Host: host, Port: port, Enabled: true}
	if err := s.clients.Create(ctx, client); err != nil {
		t.Fatal(err)
	}
	indexers, err := s.indexers.List(ctx)
	if err != nil || len(indexers) != 1 {
		t.Fatalf("indexer for stall blocklist: %+v %v", indexers, err)
	}
	dl := &models.Download{GUID: "stale-stall", Title: "Dune [ENG / EPUB]", BookID: &book.ID,
		VariantID: &copies[0].ID, IndexerID: &indexers[0].ID, DownloadClientID: &client.ID,
		TorrentID: &hash, Status: models.StateGrabbed, Protocol: "torrent"}
	if err := downloads.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := downloads.UpdateStatus(ctx, dl.ID, models.StateDownloading); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.books.VariantRepo().MarkDownloading(ctx, copies[0].ID); err != nil || !ok {
		t.Fatalf("mark downloading: %t %v", ok, err)
	}
	if err := downloads.SetGrabbedAt(ctx, dl.ID, time.Now().UTC().Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}

	s.checkStalledDownloads(ctx)
	s.bgWg.Wait()

	retained, err := downloads.GetByID(ctx, dl.ID)
	if err != nil || retained == nil || retained.Status != models.StateImportBlocked || retained.ErrorMessage != "file list unavailable" || retained.TorrentID == nil || *retained.TorrentID != hash {
		t.Fatalf("stale stall changed parked torrent: %+v %v", retained, err)
	}
	variants, err := s.books.VariantRepo().ListByBook(ctx, book.ID)
	if err != nil || len(variants) != 1 || variants[0].Status != models.VariantWanted {
		t.Errorf("stale stall undid named new-release choice: %+v %v", variants, err)
	}
	if blocked, err := s.blocklist.IsBlocked(ctx, dl.GUID); err != nil || blocked {
		t.Errorf("stale stall blocklisted release: %t %v", blocked, err)
	}
	for _, eventType := range []string{models.HistoryEventDownloadStalled, models.HistoryEventDownloadRequeued} {
		events, err := s.history.ListByType(ctx, eventType)
		if err != nil || len(events) != 0 {
			t.Errorf("stale stall wrote %s history: %+v %v", eventType, events, err)
		}
	}
	search.mu.Lock()
	searches := len(search.calls)
	search.mu.Unlock()
	spy.mu.Lock()
	webhooks := len(spy.calls)
	spy.mu.Unlock()
	if searches != 0 || *sends != 0 || unexpected.Load() != 0 || webhooks != 0 {
		t.Fatalf("stale stall triggered search/send/webhook or changed client: searches=%d sends=%d webhooks=%d unexpected=%d", searches, *sends, webhooks, unexpected.Load())
	}
}

// A terminal row must not be removed from a non-qBittorrent client even when
// the stall handler still holds its old downloading snapshot.
func TestStallHandlerRefusedTransitionDoesNotRemoveClientJob(t *testing.T) {
	s, _, downloads, _, _ := variantGrabFixture(t)
	ctx := context.Background()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "success", "arguments": map[string]any{}})
	}))
	t.Cleanup(server.Close)
	host, port := stallServerHostPort(t, server.URL)
	client := &models.DownloadClient{Name: "transmission", Type: "transmission", Host: host, Port: port, Enabled: true}
	if err := s.clients.Create(ctx, client); err != nil {
		t.Fatal(err)
	}
	hash := "stale-job-hash"
	dl := &models.Download{GUID: "stale-client-job", Title: "Old job", TorrentID: &hash,
		DownloadClientID: &client.ID, Status: models.StateGrabbed, Protocol: "torrent"}
	if err := downloads.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := downloads.UpdateStatus(ctx, dl.ID, models.StateDownloading); err != nil {
		t.Fatal(err)
	}
	snapshot, err := downloads.GetByID(ctx, dl.ID)
	if err != nil || snapshot == nil {
		t.Fatalf("snapshot download: %+v %v", snapshot, err)
	}
	for _, next := range []models.DownloadState{models.StateCompleted, models.StateImportPending, models.StateImporting, models.StateImported} {
		if err := downloads.UpdateStatus(ctx, dl.ID, next); err != nil {
			t.Fatal(err)
		}
	}
	s.handleStalledDownload(ctx, snapshot, client, downloader.StallClientReported)
	if got := requests.Load(); got != 0 {
		t.Errorf("stale stall contacted the client %d times", got)
	}
	retained, err := downloads.GetByID(ctx, dl.ID)
	if err != nil || retained == nil || retained.Status != models.StateImported {
		t.Fatalf("stale stall changed imported row: %+v %v", retained, err)
	}
}
