package importer

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

// Exercise the real downloading -> completed -> import transition against a
// read-only fake qBittorrent. An unexpected endpoint is a torrent mutation.
func terminalQbitDownload(t *testing.T, f *variantReviewFixture, title, source, contentPath string, fileListError *bool) (*models.Download, *int) {
	t.Helper()
	const hash = "abcdef1234567890abcdef1234567890abcdef12"
	mutators := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"hash": hash, "name": title, "category": "mam", "state": "stalledUP",
				"progress": 1.0, "size": 8192, "amount_left": 0, "save_path": f.sources, "content_path": contentPath}})
		case "/api/v2/torrents/files":
			if fileListError != nil && *fileListError {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"name": filepath.ToSlash(strings.TrimPrefix(source, f.sources+string(filepath.Separator))),
				"size": 8192, "progress": 1.0}})
		default:
			(*mutators)++
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	host, port := scannerTestHostPort(t, srv.URL)
	client := &models.DownloadClient{Name: "test-qbit", Type: "qbittorrent", Host: host, Port: port, Enabled: true, Category: "mam"}
	if err := db.NewDownloadClientRepo(f.database).Create(f.ctx, client); err != nil {
		t.Fatal(err)
	}
	dl := &models.Download{GUID: "terminal-status-" + hash, Title: title, BookID: &f.book.ID, VariantID: &f.variant.ID,
		DownloadClientID: &client.ID, Status: models.StateGrabbed, Protocol: "torrent"}
	if err := f.downloads.Create(f.ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := f.downloads.SetTorrentID(f.ctx, dl.ID, hash); err != nil {
		t.Fatal(err)
	}
	if err := f.downloads.UpdateStatus(f.ctx, dl.ID, models.StateDownloading); err != nil {
		t.Fatal(err)
	}
	if marked, err := f.books.VariantRepo().MarkDownloading(f.ctx, f.variant.ID); err != nil || !marked {
		t.Fatalf("real downloading variant: %t %v", marked, err)
	}
	return dl, mutators
}

func assertTerminalSourceUnchanged(t *testing.T, source string, before []byte, mutators *int) {
	t.Helper()
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("seeding source was changed: %v", err)
	}
	if *mutators != 0 {
		t.Fatalf("qBittorrent mutator/unexpected calls: %d", *mutators)
	}
}

func TestVariantTerminalQbitMissingFileListReconcilesAfterRetries(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	missingList := true
	dl, mutators := terminalQbitDownload(t, f, "English catalogue label [ENG / EPUB]", source, source, &missingList)
	client, err := db.NewDownloadClientRepo(f.database).GetByID(f.ctx, *dl.DownloadClientID)
	if err != nil {
		t.Fatal(err)
	}
	f.scanner.checkQbittorrentDownloads(f.ctx, client)
	got, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || got == nil || got.Status != models.StateImportFailed || !strings.Contains(got.ErrorMessage, "file list unavailable") {
		t.Fatalf("missing Files list is retryable: %+v %v", got, err)
	}
	if status := twoKindleVariantStatus(t, f); status != models.VariantDownloading {
		t.Fatalf("active import retry lost downloading claim: %q", status)
	}
	// The same read failure runs out of its automatic budget. The torrent is
	// still available and the download's Retry import action remains visible.
	for attempt := 0; attempt < importRetryLimit; attempt++ {
		f.scanner.checkQbittorrentDownloads(f.ctx, client)
	}
	variantBlocked(t, f, dl, "retry limit reached")
	if status := twoKindleVariantStatus(t, f); status != models.VariantReview {
		t.Fatalf("exhausted import stranded variant %q", status)
	}
	f.assertLibraryFiles(t, 0)
	active, err := f.downloads.ListActive(f.ctx, got.AddedAt.Add(-1))
	if err != nil || len(active) != 1 || active[0].ID != dl.ID || active[0].TorrentID == nil {
		t.Fatalf("failed torrent missing from actionable queue: %+v %v", active, err)
	}
	assertTerminalSourceUnchanged(t, source, before, mutators)
	// The named existing-torrent action re-arms both rows atomically. It
	// never exposes wanted or sends/replaces the seed.
	approved, err := f.downloads.RetryReviewedImport(f.ctx, f.book.ID, f.variant.ID, dl.ID, 42, "qBittorrent Files list recovered")
	if err != nil || !approved {
		t.Fatalf("rearm existing torrent: %t %v", approved, err)
	}
	if status := twoKindleVariantStatus(t, f); status != models.VariantDownloading {
		t.Fatalf("named retry left variant %q", status)
	}
	missingList = false
	f.scanner.checkQbittorrentDownloads(f.ctx, client)
	f.assertVariantPlaced(t, dl)
	assertTerminalSourceUnchanged(t, source, before, mutators)
}

func TestVariantTerminalQbitMissingLibraryRootNeedsReview(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	dl, mutators := terminalQbitDownload(t, f, "English catalogue label [ENG / EPUB]", source, source, nil)
	client, err := db.NewDownloadClientRepo(f.database).GetByID(f.ctx, *dl.DownloadClientID)
	if err != nil {
		t.Fatal(err)
	}
	f.scanner.libraryDir = "" // configuration drift must not strand completed downloads
	f.scanner.checkQbittorrentDownloads(f.ctx, client)
	variantBlocked(t, f, dl, "no library directory")
	if status := twoKindleVariantStatus(t, f); status != models.VariantReview {
		t.Fatalf("missing library root stranded variant %q", status)
	}
	f.assertLibraryFiles(t, 0)
	assertTerminalSourceUnchanged(t, source, before, mutators)
}

func TestVariantTerminalQbitCollisionKeepsTorrentAndRequestsReview(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	dest := variantEbookTarget(t, f, source)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("other file"), 0o600); err != nil {
		t.Fatal(err)
	}
	dl, mutators := terminalQbitDownload(t, f, "English catalogue label [ENG / EPUB]", source, source, nil)
	client, err := db.NewDownloadClientRepo(f.database).GetByID(f.ctx, *dl.DownloadClientID)
	if err != nil {
		t.Fatal(err)
	}
	f.scanner.checkQbittorrentDownloads(f.ctx, client)
	variantBlocked(t, f, dl, "no-replace")
	if status := twoKindleVariantStatus(t, f); status != models.VariantReview {
		t.Fatalf("collision stranded variant %q", status)
	}
	if rows, err := f.books.ListFiles(f.ctx, f.book.ID); err != nil || len(rows) != 0 {
		t.Fatalf("collision tracked another owner's file: %+v %v", rows, err)
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != "other file" {
		t.Fatalf("collision replaced existing library file: %q %v", data, err)
	}
	assertTerminalSourceUnchanged(t, source, before, mutators)
}

func TestVariantTerminalQbitFailedDBTransitionDoesNotClaimFailure(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	dest := variantEbookTarget(t, f, source)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("other file"), 0o600); err != nil {
		t.Fatal(err)
	}
	dl, mutators := terminalQbitDownload(t, f, "English catalogue label [ENG / EPUB]", source, source, nil)
	if _, err := f.database.ExecContext(f.ctx, `CREATE TRIGGER reject_terminal_review BEFORE UPDATE OF status ON book_variants
		BEGIN SELECT RAISE(ABORT, 'reconcile rejected'); END`); err != nil {
		t.Fatal(err)
	}
	client, err := db.NewDownloadClientRepo(f.database).GetByID(f.ctx, *dl.DownloadClientID)
	if err != nil {
		t.Fatal(err)
	}
	f.scanner.checkQbittorrentDownloads(f.ctx, client)
	got, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || got == nil || got.Status != models.StateImporting || got.ErrorMessage != "" {
		t.Fatalf("DB failure partially persisted terminal download: %+v %v", got, err)
	}
	if status := twoKindleVariantStatus(t, f); status != models.VariantDownloading {
		t.Fatalf("DB failure partially reconciled variant: %q", status)
	}
	events, err := f.history.ListByBook(f.ctx, f.book.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.EventType == models.HistoryEventImportFailed {
			t.Fatalf("failure history published for a rolled-back transition: %+v", event)
		}
	}
	assertTerminalSourceUnchanged(t, source, before, mutators)
}

func TestVariantTerminalQbitWrongWorkPreservesRejectionAndReview(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
	folder := filepath.Join(f.sources, "recording")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(folder, "Dune.m4b")
	before := mp4File(atom("\xa9alb", dataAtom(1, []byte("Dreamer of Dune: The Biography of Frank Herbert"))),
		atom("\xa9ART", dataAtom(1, []byte("Brian Herbert"))))
	if err := os.WriteFile(source, before, 0o644); err != nil {
		t.Fatal(err)
	}
	dl, mutators := terminalQbitDownload(t, f,
		"Dreamer of Dune: The Biography of Frank Herbert by Brian Herbert [ENG / M4B]", source, folder, nil)
	client, err := db.NewDownloadClientRepo(f.database).GetByID(f.ctx, *dl.DownloadClientID)
	if err != nil {
		t.Fatal(err)
	}
	f.scanner.checkQbittorrentDownloads(f.ctx, client)
	variantBlocked(t, f, dl, "different work")
	f.assertReview(t, folder)
	assertTerminalSourceUnchanged(t, source, before, mutators)
}
