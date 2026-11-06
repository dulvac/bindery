package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/httpsec"
	"github.com/vavallee/bindery/internal/importer"
	"github.com/vavallee/bindery/internal/indexer"
	"github.com/vavallee/bindery/internal/indexer/newznab"
	"github.com/vavallee/bindery/internal/models"
)

type variantResultsSearcher struct {
	mu      sync.Mutex
	calls   []indexer.MatchCriteria
	results map[string][]newznab.SearchResult
}

func (s *variantResultsSearcher) SearchBook(_ context.Context, _ []models.Indexer, crit indexer.MatchCriteria) []newznab.SearchResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, crit)
	return s.results[crit.MediaType+":"+crit.RequestedLanguage]
}

func variantGrabFixture(t *testing.T) (*Scheduler, *variantResultsSearcher, *db.DownloadRepo, *models.Book, *int) {
	t.Helper()
	t.Cleanup(httpsec.AllowLoopbackForTests())
	sends := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".nzb") {
			w.Header().Set("Content-Type", "application/x-nzb")
			_, _ = w.Write([]byte(`<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"></nzb>`))
			return
		}
		if r.URL.Query().Get("mode") == "addfile" {
			sends++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": true, "nzo_ids": []string{"test-nzo"}})
	}))
	t.Cleanup(server.Close)
	f := newWantedQueueFixture(t)
	book := f.book("Dune", models.MediaTypeBoth)
	indexers := db.NewIndexerRepo(f.database)
	idx := &models.Indexer{Name: "test", Type: "newznab", URL: server.URL, Enabled: true}
	if err := indexers.Create(f.ctx, idx); err != nil {
		t.Fatal(err)
	}
	host, port := stallServerHostPort(t, server.URL)
	clients := db.NewDownloadClientRepo(f.database)
	if err := clients.Create(f.ctx, &models.DownloadClient{Name: "sab", Type: "sabnzbd", Host: host, Port: port, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	search := &variantResultsSearcher{results: map[string][]newznab.SearchResult{}}
	for _, tc := range []struct{ format, lang, guid, title string }{
		{models.MediaTypeEbook, "eng", "ebook-en", "Dune [ENG / EPUB]"},
		{models.MediaTypeEbook, "rum", "ebook-ro", "Dune [ROM / EPUB]"},
		{models.MediaTypeAudiobook, "eng", "audio-en", "Dune [ENG / M4B]"},
	} {
		search.results[tc.format+":"+tc.lang] = []newznab.SearchResult{{
			GUID: tc.guid, Title: tc.title, NZBURL: server.URL + "/" + tc.guid + ".nzb",
			Protocol: "usenet", IndexerID: idx.ID, Size: 10 << 20,
		}}
	}
	s := &Scheduler{books: f.books, authors: db.NewAuthorRepo(f.database), indexers: indexers,
		searcher: search, settings: db.NewSettingsRepo(f.database), blocklist: db.NewBlocklistRepo(f.database),
		clients: clients, downloads: f.downloads, history: db.NewHistoryRepo(f.database), pending: db.NewPendingReleaseRepo(f.database)}
	return s, search, f.downloads, book, &sends
}

func TestVariantGrabEachWantedLanguageAndFormatOnce(t *testing.T) {
	s, search, downloads, book, sends := variantGrabFixture(t)
	ctx := context.Background()
	variants := s.books.VariantRepo()
	if _, err := variants.EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng", "rum"}); err != nil {
		t.Fatal(err)
	}
	if _, err := variants.EnsureWanted(ctx, book.ID, models.MediaTypeAudiobook, []string{"eng"}); err != nil {
		t.Fatal(err)
	}
	english := search.results[models.MediaTypeEbook+":eng"][0]
	romanian := search.results[models.MediaTypeEbook+":rum"][0]
	// The fake indexer returns a known wrong language FIRST on each leg.
	// Filtering, not the fake's answer order, must decide which one is sent.
	search.results[models.MediaTypeEbook+":eng"] = []newznab.SearchResult{romanian, english}
	search.results[models.MediaTypeEbook+":rum"] = []newznab.SearchResult{english, romanian}
	s.SearchAndGrabBook(ctx, *book)
	rows, err := downloads.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || *sends != 3 || len(search.calls) != 3 {
		t.Fatalf("grabs=%d sends=%d searches=%d; want three independent variants", len(rows), *sends, len(search.calls))
	}
	byID := make(map[int64]models.BookVariant)
	current, err := variants.ListByBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range current {
		byID[v.ID] = v
	}
	for _, row := range rows {
		if row.VariantID == nil {
			t.Fatalf("download %s has no variant ID", row.GUID)
		}
		v := byID[*row.VariantID]
		if v.Status != models.VariantDownloading {
			t.Errorf("%s: variant status %q, want downloading", row.GUID, v.Status)
		}
		switch row.GUID {
		case "ebook-en":
			if v.Language != "eng" || v.MediaType != models.MediaTypeEbook {
				t.Errorf("English ebook assigned to %+v", v)
			}
		case "ebook-ro":
			if v.Language != "rum" || v.MediaType != models.MediaTypeEbook {
				t.Errorf("Romanian ebook assigned to %+v", v)
			}
		case "audio-en":
			if v.Language != "eng" || v.MediaType != models.MediaTypeAudiobook {
				t.Errorf("English audio assigned to %+v", v)
			}
		default:
			t.Errorf("unexpected grab %s", row.GUID)
		}
	}
	// The book is still Wanted while its copies download; a second immediate
	// invocation or scheduled sweep must not spend further searches or grabs.
	s.SearchAndGrabBook(ctx, *book)
	if len(search.calls) != 3 || *sends != 3 {
		t.Errorf("second search made %d calls and %d sends", len(search.calls), *sends)
	}
	if got := s.wantedSearchQueue(ctx); len(got) != 0 {
		t.Errorf("downloading variants queued for next sweep: %+v", got)
	}
}

// A failed torrent still exists in the client's queue. Terminal import
// review must not make a new MAM grab on the very next wanted sweep.
func TestVariantTerminalImportReviewDoesNotTriggerDuplicateGrab(t *testing.T) {
	s, search, downloads, book, sends := variantGrabFixture(t)
	ctx := context.Background()
	copies, err := s.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	dl := &models.Download{GUID: "terminal-grab", Title: "Dune [ENG / EPUB]", BookID: &book.ID,
		VariantID: &copies[0].ID, Protocol: "torrent", Status: models.StateGrabbed}
	if err := downloads.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := downloads.SetTorrentID(ctx, dl.ID, "still-seeding"); err != nil {
		t.Fatal(err)
	}
	if err := downloads.UpdateStatus(ctx, dl.ID, models.StateDownloading); err != nil {
		t.Fatal(err)
	}
	if marked, err := s.books.VariantRepo().MarkDownloading(ctx, copies[0].ID); err != nil || !marked {
		t.Fatalf("mark real grab downloading: %t %v", marked, err)
	}
	for _, next := range []models.DownloadState{models.StateCompleted, models.StateImportPending, models.StateImporting} {
		if err := downloads.UpdateStatus(ctx, dl.ID, next); err != nil {
			t.Fatal(err)
		}
	}
	if err := downloads.SetErrorWithStatus(ctx, dl.ID, models.StateImportBlocked, "cannot place file"); err != nil {
		t.Fatal(err)
	}
	variants, err := s.books.VariantRepo().ListByBook(ctx, book.ID)
	if err != nil || len(variants) != 1 || variants[0].Status != models.VariantReview {
		t.Fatalf("blocked torrent needs review, not a stranded downloading variant: %+v %v", variants, err)
	}
	if queue := s.wantedSearchQueue(ctx); len(queue) != 0 {
		t.Fatalf("blocked torrent queued another grab: %+v", queue)
	}
	s.SearchAndGrabBook(ctx, *book)
	rows, err := downloads.List(ctx)
	if err != nil || len(rows) != 1 || rows[0].Status != models.StateImportBlocked || rows[0].TorrentID == nil || *rows[0].TorrentID != "still-seeding" ||
		len(search.calls) != 0 || *sends != 0 {
		t.Fatalf("duplicate search/grab after blocked torrent: rows=%+v calls=%d sends=%d err=%v", rows, len(search.calls), *sends, err)
	}
}

// Sweep explicitly at both sides of the named old-torrent action. The prior
// two-call path could expose wanted between review retry and import re-arming;
// neither boundary here may spend a fresh search or send.
func TestVariantExistingTorrentRetryInterleavedWantedSweepsNeverGrab(t *testing.T) {
	s, search, downloads, book, sends := variantGrabFixture(t)
	ctx := context.Background()
	copies, err := s.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	client := &models.DownloadClient{Name: "retained-qbit", Type: "qbittorrent", Host: "127.0.0.1", Port: 1}
	if err := s.clients.Create(ctx, client); err != nil {
		t.Fatal(err)
	}
	hash := "abcdef1234567890abcdef1234567890abcdef12"
	dl := &models.Download{GUID: "retry-existing-torrent", Title: "Dune [ENG / EPUB]", BookID: &book.ID,
		VariantID: &copies[0].ID, DownloadClientID: &client.ID, TorrentID: &hash, Protocol: "torrent", Status: models.StateGrabbed}
	if err := downloads.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := downloads.UpdateStatus(ctx, dl.ID, models.StateDownloading); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.books.VariantRepo().MarkDownloading(ctx, copies[0].ID); err != nil || !ok {
		t.Fatalf("mark old grab: %t %v", ok, err)
	}
	for _, next := range []models.DownloadState{models.StateCompleted, models.StateImportPending, models.StateImporting} {
		if err := downloads.UpdateStatus(ctx, dl.ID, next); err != nil {
			t.Fatal(err)
		}
	}
	if err := downloads.SetErrorWithStatus(ctx, dl.ID, models.StateImportBlocked, "file list unavailable"); err != nil {
		t.Fatal(err)
	}
	assertNoGrab := func(phase string, status models.DownloadState) {
		t.Helper()
		if queue := s.wantedSearchQueue(ctx); len(queue) != 0 {
			t.Fatalf("%s: sweep queued new release: %+v", phase, queue)
		}
		s.SearchAndGrabBook(ctx, *book)
		rows, err := downloads.List(ctx)
		if err != nil || len(rows) != 1 || rows[0].Status != status || rows[0].TorrentID == nil || *rows[0].TorrentID != hash || len(search.calls) != 0 || *sends != 0 {
			t.Fatalf("%s: old torrent changed or new release sent: rows=%+v searches=%d sends=%d err=%v", phase, rows, len(search.calls), *sends, err)
		}
	}
	assertNoGrab("before named retry", models.StateImportBlocked)
	accepted, found, err := downloads.ResetImportRetry(ctx, dl.ID)
	if err != nil || accepted || !found {
		t.Fatalf("unnamed queue retry bypassed review: %t %t %v", accepted, found, err)
	}
	approved, err := downloads.RetryReviewedImport(ctx, book.ID, copies[0].ID, dl.ID, 42, "old torrent file list recovered")
	if err != nil || !approved {
		t.Fatalf("atomic named retry: %t %v", approved, err)
	}
	assertNoGrab("after named retry", models.StateImportFailed)
	current, err := s.books.VariantRepo().ListByBook(ctx, book.ID)
	if err != nil || len(current) != 1 || current[0].Status != models.VariantDownloading {
		t.Fatalf("named retry exposed wanted between sweeps: %+v %v", current, err)
	}
}

// A normal client poll between the admin's new-release decision and the
// wanted sweep must not let the exhausted old torrent undo that decision.
func TestVariantExhaustedReviewedNewReleaseSurvivesQbitPollBeforeWantedSweep(t *testing.T) {
	s, search, downloads, book, sends := variantGrabFixture(t)
	ctx := context.Background()
	copies, err := s.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	hash := "abcdef1234567890abcdef1234567890abcdef12"
	source := filepath.Join(t.TempDir(), "old.epub")
	if err := os.WriteFile(source, []byte("old seed remains here"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mutators atomic.Int32
	var qbitError atomic.Bool
	qbit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			state := "stalledUP"
			if qbitError.Load() {
				state = "error"
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"hash": hash, "name": "old.epub", "category": "mam",
				"state": state, "progress": 1.0, "size": 8192, "amount_left": 0,
				"save_path": filepath.Dir(source), "content_path": source}})
		default:
			mutators.Add(1) // Only the torrent listing and auth are expected from this poll.
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(qbit.Close)
	host, port := stallServerHostPort(t, qbit.URL)
	client := &models.DownloadClient{Name: "old-qbit", Type: "qbittorrent", Host: host, Port: port, Category: "mam", Enabled: true}
	if err := s.clients.Create(ctx, client); err != nil {
		t.Fatal(err)
	}
	dl := &models.Download{GUID: "old-exhausted-torrent", Title: "Dune [ENG / EPUB]", BookID: &book.ID,
		VariantID: &copies[0].ID, DownloadClientID: &client.ID, TorrentID: &hash, Protocol: "torrent", Status: models.StateGrabbed}
	if err := downloads.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := downloads.UpdateStatus(ctx, dl.ID, models.StateDownloading); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.books.VariantRepo().MarkDownloading(ctx, copies[0].ID); err != nil || !ok {
		t.Fatalf("old grab not downloading: %t %v", ok, err)
	}
	for _, next := range []models.DownloadState{models.StateCompleted, models.StateImportPending, models.StateImporting} {
		if err := downloads.UpdateStatus(ctx, dl.ID, next); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < db.ImportRetryLimit; i++ {
		if err := downloads.IncrementImportRetryCount(ctx, dl.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := downloads.RecoverInterruptedImports(ctx); err != nil {
		t.Fatal(err)
	}
	if queue := s.wantedSearchQueue(ctx); len(queue) != 0 {
		t.Fatalf("review queued an unattended grab: %+v", queue)
	}
	if err := s.books.RejectionRepo().Record(ctx, book.ID, copies[0].ID, dl.ID, source, "old source rejected"); err != nil {
		t.Fatal(err)
	}
	approved, err := s.books.VariantRepo().RetryReviewed(ctx, book.ID, copies[0].ID, 42, "seek another release")
	if err != nil || !approved {
		t.Fatalf("new-release decision: %t %v", approved, err)
	}
	poller := importer.NewScanner(downloads, s.clients, s.books, s.authors, s.history, "", "", "", "", "")
	poller.CheckDownloads(ctx) // an ordinary qBittorrent poll, before the next wanted sweep
	queue := s.wantedSearchQueue(ctx)
	if len(queue) != 1 || queue[0].book.ID != book.ID || len(queue[0].formats) != 1 || queue[0].formats[0] != models.MediaTypeEbook {
		t.Fatalf("qBittorrent poll undid the new-release decision: %+v", queue)
	}
	// The retained torrent may later report a client error. That must not
	// overwrite its terminal import state or disable the named retry action.
	qbitError.Store(true)
	poller.CheckDownloads(ctx)
	if queue := s.wantedSearchQueue(ctx); len(queue) != 1 || queue[0].book.ID != book.ID {
		t.Fatalf("old torrent error hid new-release search: %+v", queue)
	}
	old, err := downloads.GetByID(ctx, dl.ID)
	if err != nil || old == nil || old.Status != models.StateImportBlocked || old.ImportRetryCount != db.ImportRetryLimit ||
		old.TorrentID == nil || *old.TorrentID != hash || old.DownloadClientID == nil || *old.DownloadClientID != client.ID {
		t.Fatalf("old torrent changed instead of becoming inert: %+v %v", old, err)
	}
	if rejected, err := s.books.RejectionRepo().IsRejected(ctx, book.ID, copies[0].ID, source); err != nil || !rejected {
		t.Fatalf("old rejection was lost: rejected=%t err=%v", rejected, err)
	}
	if data, err := os.ReadFile(source); err != nil || string(data) != "old seed remains here" {
		t.Fatalf("source changed: %q %v", data, err)
	}
	if len(search.calls) != 0 || *sends != 0 || mutators.Load() != 0 {
		t.Fatalf("poll/sweep sent or changed a torrent: searches=%d sends=%d qbit unexpected=%d", len(search.calls), *sends, mutators.Load())
	}
}

func TestVariantGrabImportedEnglishStillSearchesMissingRomanian(t *testing.T) {
	s, search, downloads, book, sends := variantGrabFixture(t)
	ctx := context.Background()
	variants := s.books.VariantRepo()
	copies, err := variants.EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	if err := variants.SetStatus(ctx, copies[0].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	book.MediaType = models.MediaTypeEbook
	book.Status = models.BookStatusImported
	book.EbookFilePath = "/library/Dune.epub"
	if err := s.books.Update(ctx, book); err != nil {
		t.Fatal(err)
	}
	queue := s.wantedSearchQueue(ctx)
	if len(queue) != 1 || queue[0].book.ID != book.ID {
		t.Fatalf("imported English, wanted Romanian: queue=%+v", queue)
	}
	s.SearchAndGrabBook(ctx, *book)
	rows, err := downloads.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].GUID != "ebook-ro" || *sends != 1 || len(search.calls) != 1 || search.calls[0].RequestedLanguage != "rum" {
		t.Fatalf("missing Romanian variant: downloads=%+v sends=%d criteria=%+v", rows, *sends, search.calls)
	}
}

// An explicit Romanian request must win over the author's English-only
// catalogue profile for release selection; the catalogue filter is separate.
func TestVariantGrabFailedSendLeavesWanted(t *testing.T) {
	ctx := context.Background()
	s, _, downloads, book := languageFixture(t, "eng", []newznab.SearchResult{
		{GUID: "client-down", Title: "Il Libro [ENG / EPUB]", NZBURL: "http://127.0.0.1:1/en", Protocol: "usenet"},
	})
	copies, err := s.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	s.SearchAndGrabBook(ctx, book)
	rows, err := downloads.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != models.StateFailed || rows[0].VariantID == nil || *rows[0].VariantID != copies[0].ID {
		t.Fatalf("failed send must retain its variant for audit: %+v", rows)
	}
	current, err := s.books.VariantRepo().ListByBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current[0].Status != models.VariantWanted {
		t.Errorf("failed send left copy %q; want wanted", current[0].Status)
	}
}

func TestVariantGrabRequestedLanguageOverridesNarrowerAuthorProfile(t *testing.T) {
	ctx := context.Background()
	s, search, downloads, book := languageFixture(t, "eng", []newznab.SearchResult{
		{GUID: "requested-rum", Title: "Il Libro [ROM / EPUB]", NZBURL: "http://127.0.0.1:1/ro", Protocol: "usenet"},
	})
	if _, err := s.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"rum"}); err != nil {
		t.Fatal(err)
	}
	s.SearchAndGrabBook(ctx, book)
	rows, err := downloads.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if search.lastCrit.RequestedLanguage != "rum" || len(rows) != 1 || rows[0].GUID != "requested-rum" {
		t.Fatalf("Romanian request under English profile: criteria=%+v rows=%+v", search.lastCrit, rows)
	}
}

func TestVariantGrabKeepsOtherLanguagePendingRelease(t *testing.T) {
	s, _, _, book, _ := variantGrabFixture(t)
	ctx := context.Background()
	copies, err := s.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.books.VariantRepo().SetStatus(ctx, copies[1].ID, models.VariantReview); err != nil {
		t.Fatal(err)
	}
	res := newznab.SearchResult{GUID: "held-rum", Title: "Dune [ROM / EPUB]", Protocol: "usenet"}
	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.pending.Upsert(ctx, &models.PendingRelease{BookID: book.ID, MediaType: models.MediaTypeEbook,
		GUID: res.GUID, Title: res.Title, Protocol: res.Protocol, ReleaseJSON: string(blob)}); err != nil {
		t.Fatal(err)
	}
	s.SearchAndGrabBook(ctx, *book)
	remaining, err := s.pending.ListByBookAndMediaType(ctx, book.ID, models.MediaTypeEbook)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].GUID != "held-rum" {
		t.Fatalf("English grab removed unrelated Romanian pending release: %+v", remaining)
	}
}

func TestVariantGrabImmediateRespectsLegacyUnboundDownload(t *testing.T) {
	s, search, downloads, book, sends := variantGrabFixture(t)
	ctx := context.Background()
	book.MediaType = models.MediaTypeAudiobook
	if err := s.books.Update(ctx, book); err != nil {
		t.Fatal(err)
	}
	if _, err := s.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeAudiobook, []string{"eng"}); err != nil {
		t.Fatal(err)
	}
	legacy := &models.Download{GUID: "already-fetching", BookID: &book.ID, Title: "Dune [PDF booklet]",
		Quality: "pdf", Protocol: "usenet", Status: models.StateDownloading}
	if err := downloads.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	s.SearchAndGrabBook(ctx, *book)
	if len(search.calls) != 0 || *sends != 0 {
		t.Fatalf("immediate variant search bypassed legacy in-flight hold: criteria=%+v sends=%d", search.calls, *sends)
	}
}

func TestVariantGrabLegacyUnboundDownloadBlocksSingleFormat(t *testing.T) {
	f := newWantedQueueFixture(t)
	book := f.book("Audio with booklet", models.MediaTypeAudiobook)
	if _, err := f.books.VariantRepo().EnsureWanted(f.ctx, book.ID, models.MediaTypeAudiobook, []string{"eng"}); err != nil {
		t.Fatal(err)
	}
	// The legacy download has no variant and its PDF booklet makes it look
	// like an ebook even though this book has only an audiobook slot.
	f.download(book, "pdf", models.StateDownloading)
	if queue := f.queue(); len(queue) != 0 {
		t.Fatalf("legacy unbound download led to a duplicate single-format search: %+v", queue)
	}
}

func TestVariantGrabImportRetryDoesNotSpendAnotherQuery(t *testing.T) {
	f := newWantedQueueFixture(t)
	book := f.book("Retrying import", models.MediaTypeEbook)
	copies, err := f.books.VariantRepo().EnsureWanted(f.ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.downloads.Create(f.ctx, &models.Download{GUID: "retrying-import", BookID: &book.ID,
		VariantID: &copies[0].ID, Title: "Retrying import [ENG / EPUB]", Protocol: "usenet", Status: models.StateImportFailed}); err != nil {
		t.Fatal(err)
	}
	if queue := f.queue(); len(queue) != 0 {
		t.Fatalf("import retry still working, but scheduler queued another query: %+v", queue)
	}
}

func TestVariantGrabIgnoresVariantOutsideBooksFormat(t *testing.T) {
	s, search, downloads, book, sends := variantGrabFixture(t)
	ctx := context.Background()
	book.MediaType = models.MediaTypeEbook
	if err := s.books.Update(ctx, book); err != nil {
		t.Fatal(err)
	}
	if _, err := s.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeAudiobook, []string{"eng"}); err != nil {
		t.Fatal(err)
	}
	s.SearchAndGrabBook(ctx, *book)
	rows, err := downloads.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || len(search.calls) != 0 || *sends != 0 {
		t.Fatalf("no audiobook requested by the book: downloads=%+v criteria=%+v sends=%d", rows, search.calls, *sends)
	}
}

func TestVariantGrabCapAndDisableLeaveWanted(t *testing.T) {
	s, search, downloads, book, sends := variantGrabFixture(t)
	ctx := context.Background()
	copies, err := s.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	// A quota-exhausted searcher returns no releases: the scheduler must not
	// claim the copy or send an alternative language.
	search.results[models.MediaTypeEbook+":rum"] = nil
	s.SearchAndGrabBook(ctx, *book)
	current, err := s.books.VariantRepo().ListByBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].ID != copies[0].ID || current[0].Status != models.VariantWanted {
		t.Fatalf("quota held copy: %+v", current)
	}
	if err := s.settings.Set(ctx, "autoGrab.enabled", "false"); err != nil {
		t.Fatal(err)
	}
	search.results[models.MediaTypeEbook+":rum"] = []newznab.SearchResult{{GUID: "ebook-ro", Title: "Dune [ROM / EPUB]"}}
	s.SearchAndGrabBook(ctx, *book)
	rows, err := downloads.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || *sends != 0 || len(search.calls) != 1 {
		t.Fatalf("disabled search: rows=%+v sends=%d calls=%d", rows, *sends, len(search.calls))
	}
}
