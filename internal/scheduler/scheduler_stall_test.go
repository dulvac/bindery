package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/downloader"
	"github.com/vavallee/bindery/internal/indexer"
	"github.com/vavallee/bindery/internal/models"
)

func stallServerHostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}
	return host, port
}

// TestWithHistory_Setter confirms WithHistory stores the repo.
func TestWithHistory_Setter(t *testing.T) {
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer database.Close()

	s := &Scheduler{}
	if s.history != nil {
		t.Fatal("history should start nil")
	}
	h := db.NewHistoryRepo(database)
	s.WithHistory(h)
	if s.history != h {
		t.Error("WithHistory did not assign history repo")
	}
}

// TestStart_RegistersStallJob verifies that the new stall-check job is
// among the cron entries registered at Start.
func TestStart_RegistersStallJob(t *testing.T) {
	s := &Scheduler{
		cron: cron.New(cron.WithSeconds()),
	}
	s.Start()
	entries := s.cron.Entries()
	s.Stop()

	if len(entries) != 5 {
		t.Errorf("expected 5 cron entries after Start() (including stall check), got %d", len(entries))
	}
}

// TestCheckStalledDownloads_NoActiveDownloads returns early when there are no
// downloads in "downloading" status.
func TestCheckStalledDownloads_NoActiveDownloads(t *testing.T) {
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer database.Close()

	s := &Scheduler{
		downloads: db.NewDownloadRepo(database),
		settings:  db.NewSettingsRepo(database),
	}
	s.checkStalledDownloads(context.Background())
}

// TestCheckStalledDownloads_SkipsNewDownloads verifies that downloads whose
// GrabbedAt is more recent than (now - timeout) are skipped. We use a
// 24-hour timeout so freshly-inserted downloads are below the cutoff.
func TestCheckStalledDownloads_SkipsNewDownloads(t *testing.T) {
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer database.Close()
	ctx := context.Background()

	settingsRepo := db.NewSettingsRepo(database)
	if err := settingsRepo.Set(ctx, "stall.timeout_minutes", "1440"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	downloadsRepo := db.NewDownloadRepo(database)
	clientsRepo := db.NewDownloadClientRepo(database)

	client := &models.DownloadClient{
		Name: "qb", Type: "qbittorrent", Host: "127.0.0.1", Port: 1,
		Username: "u", Password: "p", Enabled: true,
	}
	if err := clientsRepo.Create(ctx, client); err != nil {
		t.Fatalf("create client: %v", err)
	}

	dl := &models.Download{
		GUID: "g1", Title: "fresh", DownloadClientID: &client.ID,
		Status: models.StateGrabbed, Protocol: "torrent",
	}
	if err := downloadsRepo.Create(ctx, dl); err != nil {
		t.Fatalf("create download: %v", err)
	}
	if err := downloadsRepo.UpdateStatus(ctx, dl.ID, models.DownloadStatusDownloading); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	s := &Scheduler{
		downloads: downloadsRepo,
		clients:   clientsRepo,
		settings:  settingsRepo,
	}
	s.checkStalledDownloads(ctx)
}

// TestCheckStalledDownloads_SkipsNilGrabbedAt verifies that downloads with
// DownloadClientID set but no GrabbedAt are skipped.
func TestCheckStalledDownloads_SkipsNilGrabbedAt(t *testing.T) {
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer database.Close()
	ctx := context.Background()

	downloadsRepo := db.NewDownloadRepo(database)
	clientsRepo := db.NewDownloadClientRepo(database)

	client := &models.DownloadClient{
		Name: "qb", Type: "qbittorrent", Host: "127.0.0.1", Port: 1,
		Username: "u", Password: "p", Enabled: true,
	}
	if err := clientsRepo.Create(ctx, client); err != nil {
		t.Fatalf("create client: %v", err)
	}

	dl := &models.Download{
		GUID: "g-nil-grab", Title: "nil grab", DownloadClientID: &client.ID,
		Status: models.DownloadStatusDownloading, Protocol: "torrent",
	}
	if err := downloadsRepo.Create(ctx, dl); err != nil {
		t.Fatalf("create download: %v", err)
	}

	s := &Scheduler{
		downloads: downloadsRepo,
		clients:   clientsRepo,
		settings:  db.NewSettingsRepo(database),
	}
	s.checkStalledDownloads(ctx)
}

// TestCheckStalledDownloads_DisabledClientSkipped verifies that disabled
// clients short-circuit without a network call.
func TestCheckStalledDownloads_DisabledClientSkipped(t *testing.T) {
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer database.Close()
	ctx := context.Background()

	downloadsRepo := db.NewDownloadRepo(database)
	clientsRepo := db.NewDownloadClientRepo(database)

	client := &models.DownloadClient{
		Name: "qb", Type: "qbittorrent", Host: "127.0.0.1", Port: 1,
		Username: "u", Password: "p", Enabled: false,
	}
	if err := clientsRepo.Create(ctx, client); err != nil {
		t.Fatalf("create client: %v", err)
	}

	old := time.Now().UTC().Add(-3 * time.Hour)
	dl := &models.Download{
		GUID: "g-old", Title: "old", DownloadClientID: &client.ID,
		Status: models.StateGrabbed, Protocol: "torrent",
	}
	if err := downloadsRepo.Create(ctx, dl); err != nil {
		t.Fatalf("create download: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		"UPDATE downloads SET status=?, grabbed_at=? WHERE id=?",
		models.DownloadStatusDownloading, old, dl.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	s := &Scheduler{
		downloads: downloadsRepo,
		clients:   clientsRepo,
		settings:  db.NewSettingsRepo(database),
	}
	s.checkStalledDownloads(ctx)
}

// TestCheckStalledDownloads_QBitNoStalls exercises the full qBittorrent
// path with a server that reports no stalled torrents.
func TestCheckStalledDownloads_QBitNoStalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"hash":  "ABCDEF",
				"state": "downloading",
			}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	host, port := stallServerHostPort(t, srv.URL)

	database, err := db.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer database.Close()
	ctx := context.Background()

	clientsRepo := db.NewDownloadClientRepo(database)
	client := &models.DownloadClient{
		Name: "qb", Type: "qbittorrent", Host: host, Port: port,
		Username: "u", Password: "p", Enabled: true,
	}
	if err := clientsRepo.Create(ctx, client); err != nil {
		t.Fatalf("create client: %v", err)
	}

	downloadsRepo := db.NewDownloadRepo(database)
	tid := "abcdef"
	dl := &models.Download{
		GUID: "g1", Title: "Active Book", DownloadClientID: &client.ID,
		Status: models.StateGrabbed, Protocol: "torrent",
		TorrentID: &tid,
	}
	if err := downloadsRepo.Create(ctx, dl); err != nil {
		t.Fatalf("create download: %v", err)
	}
	old := time.Now().UTC().Add(-3 * time.Hour)
	if _, err := database.ExecContext(ctx,
		"UPDATE downloads SET status=?, grabbed_at=? WHERE id=?",
		models.DownloadStatusDownloading, old, dl.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	s := &Scheduler{
		downloads: downloadsRepo,
		clients:   clientsRepo,
		settings:  db.NewSettingsRepo(database),
		blocklist: db.NewBlocklistRepo(database),
	}
	s.checkStalledDownloads(ctx)

	got, err := downloadsRepo.GetByGUID(ctx, "g1")
	if err != nil {
		t.Fatalf("GetByGUID: %v", err)
	}
	if got.Status != models.DownloadStatusDownloading {
		t.Errorf("expected status to remain %q, got %q",
			models.DownloadStatusDownloading, got.Status)
	}
}

// stallDeleteRecorder captures the /api/v2/torrents/delete calls a fake
// qBittorrent receives, so a test can assert the stall handler actually removed
// the torrent it says it removed (#2367).
type stallDeleteRecorder struct {
	mu     sync.Mutex
	hashes []string
	files  []string
}

func (r *stallDeleteRecorder) record(req *http.Request) {
	_ = req.ParseForm()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hashes = append(r.hashes, req.PostFormValue("hashes"))
	r.files = append(r.files, req.PostFormValue("deleteFiles"))
}

func (r *stallDeleteRecorder) calls() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hashes...), append([]string(nil), r.files...)
}

// TestCheckStalledDownloads_QBitStalledTorrent walks the full path: qBit
// reports a stalledDL torrent whose hash matches an old download, so
// handleStalledDownload fires. The qBittorrent interlock keeps the torrent in
// the client (no delete call), while the row is still failed, blocklisted and
// recorded in history.
func TestCheckStalledDownloads_QBitStalledTorrent(t *testing.T) {
	deletes := &stallDeleteRecorder{}
	sourceBytes := []byte("mam-stalled-payload")
	sourceFile := filepath.Join(t.TempDir(), "stalled.epub")
	if err := os.WriteFile(sourceFile, sourceBytes, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"hash":     "DEADBEEF",
				"state":    "stalledDL",
				"category": "books", // drift from configured "mam"
			}})
		case "/api/v2/torrents/delete":
			deletes.record(r)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	host, port := stallServerHostPort(t, srv.URL)

	database, err := db.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer database.Close()
	ctx := context.Background()

	authorsRepo := db.NewAuthorRepo(database)
	booksRepo := db.NewBookRepo(database)
	author := &models.Author{ForeignID: "OLSTALL", Name: "Stall", SortName: "Stall", MetadataProvider: "ol", Monitored: true}
	if err := authorsRepo.Create(ctx, author); err != nil {
		t.Fatalf("create author: %v", err)
	}
	book := &models.Book{
		ForeignID: "OLB1", AuthorID: author.ID, Title: "Stalled Book",
		SortTitle: "Stalled Book", Status: models.BookStatusWanted,
		Genres: []string{}, MetadataProvider: "ol", Monitored: true,
	}
	if err := booksRepo.Create(ctx, book); err != nil {
		t.Fatalf("create book: %v", err)
	}

	clientsRepo := db.NewDownloadClientRepo(database)
	client := &models.DownloadClient{
		Name: "qb", Type: "qbittorrent", Host: host, Port: port,
		Username: "u", Password: "p", Enabled: true,
		Category: "mam",
	}
	if err := clientsRepo.Create(ctx, client); err != nil {
		t.Fatalf("create client: %v", err)
	}

	indexersRepo := db.NewIndexerRepo(database)
	idx := &models.Indexer{Name: "mock", Type: "newznab", URL: "http://x", APIKey: "k", Enabled: true, Priority: 25}
	if err := indexersRepo.Create(ctx, idx); err != nil {
		t.Fatalf("create indexer: %v", err)
	}

	downloadsRepo := db.NewDownloadRepo(database)
	tid := strings.ToLower("DEADBEEF")
	dl := &models.Download{
		GUID: "g-stalled", Title: "Stalled Release", BookID: &book.ID,
		IndexerID: &idx.ID, DownloadClientID: &client.ID,
		Status: models.StateGrabbed, Protocol: "torrent",
		TorrentID: &tid, ImportPath: sourceFile,
	}
	if err := downloadsRepo.Create(ctx, dl); err != nil {
		t.Fatalf("create download: %v", err)
	}
	old := time.Now().UTC().Add(-3 * time.Hour)
	if _, err := database.ExecContext(ctx,
		"UPDATE downloads SET status=?, grabbed_at=? WHERE id=?",
		models.DownloadStatusDownloading, old, dl.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	blocklistRepo := db.NewBlocklistRepo(database)
	historyRepo := db.NewHistoryRepo(database)

	// autoGrab.enabled=false prevents the re-search goroutine from firing.
	settingsRepo := db.NewSettingsRepo(database)
	_ = settingsRepo.Set(ctx, "autoGrab.enabled", "false")

	s := &Scheduler{
		downloads: downloadsRepo,
		clients:   clientsRepo,
		indexers:  indexersRepo,
		books:     booksRepo,
		authors:   authorsRepo,
		settings:  settingsRepo,
		blocklist: blocklistRepo,
		history:   historyRepo,
		searcher:  indexer.NewSearcher(),
	}
	s.checkStalledDownloads(ctx)

	hashes, _ := deletes.calls()
	if len(hashes) != 0 {
		t.Fatalf("qBittorrent interlock violated: expected 0 delete calls, got %d (%v)", len(hashes), hashes)
	}
	payload, err := os.ReadFile(sourceFile)
	if err != nil {
		t.Fatalf("read source after stall handling: %v", err)
	}
	if !bytes.Equal(payload, sourceBytes) {
		t.Fatalf("source bytes changed unexpectedly: got %q, want %q", payload, sourceBytes)
	}

	got, err := downloadsRepo.GetByGUID(ctx, "g-stalled")
	if err != nil {
		t.Fatalf("GetByGUID: %v", err)
	}
	if got.Status != models.DownloadStatusFailed {
		t.Errorf("download status: want %q, got %q",
			models.DownloadStatusFailed, got.Status)
	}
	if got.ErrorMessage == "" {
		t.Error("expected ErrorMessage to be set on stalled download")
	}

	blocked, err := blocklistRepo.IsBlocked(ctx, "g-stalled")
	if err != nil {
		t.Fatalf("IsBlocked: %v", err)
	}
	if !blocked {
		t.Error("expected GUID to be blocklisted after stall")
	}

	events, err := historyRepo.ListByType(ctx, models.HistoryEventDownloadStalled)
	if err != nil {
		t.Fatalf("ListByType: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 downloadStalled history event, got %d", len(events))
	}
	if events[0].SourceTitle != "Stalled Release" {
		t.Errorf("history source_title: want %q, got %q",
			"Stalled Release", events[0].SourceTitle)
	}
}

// TestCheckStalledDownloads_TimeoutSettingParsedBadValueIgnored verifies the
// stall.timeout_minutes setting falls back to the default when the value is
// not a positive integer.
func TestCheckStalledDownloads_TimeoutSettingParsedBadValueIgnored(t *testing.T) {
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer database.Close()
	ctx := context.Background()

	settingsRepo := db.NewSettingsRepo(database)
	_ = settingsRepo.Set(ctx, "stall.timeout_minutes", "not-a-number")

	s := &Scheduler{
		downloads: db.NewDownloadRepo(database),
		settings:  settingsRepo,
	}
	s.checkStalledDownloads(ctx)

	_ = settingsRepo.Set(ctx, "stall.timeout_minutes", "-5")
	s.checkStalledDownloads(ctx)
}

// TestHandleStalledDownload_NoHistoryRepo verifies the handler runs safely
// when no history repo is attached.
func TestHandleStalledDownload_NoHistoryRepo(t *testing.T) {
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer database.Close()
	ctx := context.Background()

	downloadsRepo := db.NewDownloadRepo(database)
	blocklistRepo := db.NewBlocklistRepo(database)
	indexersRepo := db.NewIndexerRepo(database)
	idx := &models.Indexer{Name: "mock", Type: "newznab", URL: "http://x", APIKey: "k", Enabled: true, Priority: 25}
	if err := indexersRepo.Create(ctx, idx); err != nil {
		t.Fatalf("create indexer: %v", err)
	}

	dl := &models.Download{
		GUID: "g-nb", Title: "no book", Status: models.DownloadStatusDownloading,
		Protocol: "torrent", IndexerID: &idx.ID,
	}
	if err := downloadsRepo.Create(ctx, dl); err != nil {
		t.Fatalf("create: %v", err)
	}

	s := &Scheduler{
		downloads: downloadsRepo,
		blocklist: blocklistRepo,
		settings:  db.NewSettingsRepo(database),
	}
	s.handleStalledDownload(ctx, dl, nil, downloader.StallClientReported)

	got, _ := downloadsRepo.GetByGUID(ctx, "g-nb")
	if got.Status != models.DownloadStatusFailed {
		t.Errorf("expected failed status, got %q", got.Status)
	}
	blocked, _ := blocklistRepo.IsBlocked(ctx, "g-nb")
	if !blocked {
		t.Error("expected GUID to be blocklisted")
	}
}

// TestHandleStalledDownload_NilBlocklistRepo verifies the handler doesn't
// panic when the blocklist repo is nil.
func TestHandleStalledDownload_NilBlocklistRepo(t *testing.T) {
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer database.Close()
	ctx := context.Background()

	downloadsRepo := db.NewDownloadRepo(database)
	dl := &models.Download{
		GUID: "g-nbl", Title: "no bl", Status: models.DownloadStatusDownloading,
		Protocol: "torrent",
	}
	if err := downloadsRepo.Create(ctx, dl); err != nil {
		t.Fatalf("create: %v", err)
	}

	s := &Scheduler{
		downloads: downloadsRepo,
		settings:  db.NewSettingsRepo(database),
	}
	s.handleStalledDownload(ctx, dl, nil, downloader.StallClientReported)
}

// TestCheckStalledDownloads_RemovalFailureStillRecovers keeps the non-qBit
// behaviour pinned: when another client (Transmission here) refuses removal,
// stall recovery still fails the row, blocklists the GUID and records history.
func TestCheckStalledDownloads_RemovalFailureStillRecovers(t *testing.T) {
	var deleteCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/transmission/rpc" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			Method    string         `json:"method"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode rpc body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch req.Method {
		case "torrent-get":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": "success",
				"arguments": map[string]any{"torrents": []map[string]any{{
					"id":          7,
					"hashString":  "BADDELETE",
					"status":      0,
					"errorString": "stalled forever",
					"percentDone": 0.4,
					"downloadDir": "/transmission/mam",
				}}},
			})
		case "torrent-remove":
			deleteCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"result": "torrent not found", "arguments": map[string]any{}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"result": "success", "arguments": map[string]any{}})
		}
	}))
	defer srv.Close()

	host, port := stallServerHostPort(t, srv.URL)

	database, err := db.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer database.Close()
	ctx := context.Background()

	clientsRepo := db.NewDownloadClientRepo(database)
	client := &models.DownloadClient{
		Name: "tx", Type: "transmission", Host: host, Port: port,
		Username: "u", Password: "p", Enabled: true,
	}
	if err := clientsRepo.Create(ctx, client); err != nil {
		t.Fatalf("create client: %v", err)
	}

	indexersRepo := db.NewIndexerRepo(database)
	idx := &models.Indexer{Name: "mock", Type: "newznab", URL: "http://x", APIKey: "k", Enabled: true, Priority: 25}
	if err := indexersRepo.Create(ctx, idx); err != nil {
		t.Fatalf("create indexer: %v", err)
	}

	downloadsRepo := db.NewDownloadRepo(database)
	tid := strings.ToLower("BADDELETE")
	dl := &models.Download{
		GUID: "g-baddelete", Title: "Undeletable Release",
		IndexerID: &idx.ID, DownloadClientID: &client.ID,
		Status: models.StateGrabbed, Protocol: "torrent",
		TorrentID: &tid,
	}
	if err := downloadsRepo.Create(ctx, dl); err != nil {
		t.Fatalf("create download: %v", err)
	}
	old := time.Now().UTC().Add(-3 * time.Hour)
	if _, err := database.ExecContext(ctx,
		"UPDATE downloads SET status=?, grabbed_at=? WHERE id=?",
		models.DownloadStatusDownloading, old, dl.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	blocklistRepo := db.NewBlocklistRepo(database)
	historyRepo := db.NewHistoryRepo(database)
	s := &Scheduler{
		downloads: downloadsRepo,
		clients:   clientsRepo,
		indexers:  indexersRepo,
		settings:  db.NewSettingsRepo(database),
		blocklist: blocklistRepo,
		history:   historyRepo,
	}
	s.checkStalledDownloads(ctx)

	if n := deleteCalls.Load(); n != 1 {
		t.Fatalf("expected 1 delete attempt for non-qBit client, got %d", n)
	}
	got, err := downloadsRepo.GetByGUID(ctx, "g-baddelete")
	if err != nil {
		t.Fatalf("GetByGUID: %v", err)
	}
	if got.Status != models.DownloadStatusFailed {
		t.Errorf("download status after a refused delete: want %q, got %q",
			models.DownloadStatusFailed, got.Status)
	}
	blocked, err := blocklistRepo.IsBlocked(ctx, "g-baddelete")
	if err != nil {
		t.Fatalf("IsBlocked: %v", err)
	}
	if !blocked {
		t.Error("a refused delete skipped the blocklist, so the same release can be re-grabbed")
	}
	events, err := historyRepo.ListByType(ctx, models.HistoryEventDownloadStalled)
	if err != nil {
		t.Fatalf("ListByType: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("expected 1 downloadStalled history event, got %d", len(events))
	}
}
