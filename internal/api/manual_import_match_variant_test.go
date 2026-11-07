package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

type reviewedMatchFixture struct {
	conn       *sql.DB
	h          *ManualImportHandler
	scanner    *stubManualImportScanner
	books      *db.BookRepo
	downloads  *db.DownloadRepo
	book       *models.Book
	other      *models.Book
	variant    models.BookVariant
	dl         *models.Download
	source     string
	clientURL  string
	clientHits atomic.Int32
}

func newReviewedMatchFixture(t *testing.T, status models.DownloadState, ownerMode string, withPath bool) *reviewedMatchFixture {
	t.Helper()
	conn, err := db.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	users := db.NewUserRepo(conn)
	owner, err := users.Create(ctx, "reviewed-owner", "hash")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := users.Create(ctx, "reviewed-foreign", "hash")
	if err != nil {
		t.Fatal(err)
	}
	author := &models.Author{ForeignID: "match-review-author", Name: "Frank Herbert", SortName: "Herbert, Frank"}
	if err := db.NewAuthorRepo(conn).Create(ctx, author); err != nil {
		t.Fatal(err)
	}
	books := db.NewBookRepo(conn)
	book := &models.Book{ForeignID: "match-review-dune", AuthorID: author.ID, OwnerUserID: owner.ID,
		Title: "Dune", SortTitle: "dune", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	other := &models.Book{ForeignID: "match-review-biography", AuthorID: author.ID, OwnerUserID: foreign.ID,
		Title: "Dreamer of Dune", SortTitle: "dreamer of dune", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	for _, b := range []*models.Book{book, other} {
		if err := books.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	variants, err := books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	f := &reviewedMatchFixture{conn: conn, books: books, book: book, other: other, variant: variants[0]}
	fakeClient := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.clientHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(fakeClient.Close)
	f.clientURL = fakeClient.URL
	parsed, err := url.Parse(fakeClient.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	client := &models.DownloadClient{Name: "test-only", Type: "qbittorrent", Host: parsed.Hostname(), Port: port}
	if err := db.NewDownloadClientRepo(conn).Create(ctx, client); err != nil {
		t.Fatal(err)
	}
	downloadOwner := owner.ID
	switch ownerMode {
	case "absent":
		downloadOwner = 0
	case "foreign":
		downloadOwner = foreign.ID
	}
	f.downloads = db.NewDownloadRepo(conn)
	f.dl = &models.Download{GUID: "reviewed-wrong-work", BookID: &book.ID, VariantID: &f.variant.ID,
		OwnerUserID: downloadOwner, Title: "Dune [ENG / EPUB]", Protocol: "torrent", Status: status,
		DownloadClientID: &client.ID}
	if err := f.downloads.Create(ctx, f.dl); err != nil {
		t.Fatal(err)
	}
	const hash = "abcdef1234567890abcdef1234567890abcdef12"
	if err := f.downloads.SetTorrentID(ctx, f.dl.ID, hash); err != nil {
		t.Fatal(err)
	}
	f.source = filepath.Join(t.TempDir(), "private-seed", "wrong-work.epub")
	if err := os.MkdirAll(filepath.Dir(f.source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.source, []byte("source unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if withPath {
		if err := f.downloads.SetImportPath(ctx, f.dl.ID, f.source); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE downloads SET import_retry_count=?, error_message='wrong work' WHERE id=?`, db.ImportRetryLimit, f.dl.ID); err != nil {
		t.Fatal(err)
	}
	if err := books.VariantRepo().SetStatus(ctx, f.variant.ID, models.VariantReview); err != nil {
		t.Fatal(err)
	}
	if err := books.RejectionRepo().Record(ctx, book.ID, f.variant.ID, f.dl.ID, f.source, "wrong work"); err != nil {
		t.Fatal(err)
	}
	f.scanner = &stubManualImportScanner{}
	f.h = NewManualImportHandler(f.scanner, f.downloads, books).WithRoots(NewLibraryRoots(nil, filepath.Dir(f.source)))
	return f
}

func (f *reviewedMatchFixture) match(t *testing.T, bookID int64) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"downloadId": f.dl.ID, "bookId": bookID})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.h.MatchDownload(rec, httptest.NewRequest(http.MethodPost, "/api/v1/queue/manual-import/match", bytes.NewReader(body)))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "reviewed-variant retry") {
		t.Fatalf("Match to book = %d %q, want 409 directing owner to reviewed-variant retry", rec.Code, rec.Body.String())
	}
	for _, private := range []string{f.source, "abcdef1234567890abcdef1234567890abcdef12", f.clientURL} {
		if strings.Contains(rec.Body.String(), private) {
			t.Fatalf("Match response exposed source/client identity: %q", rec.Body.String())
		}
	}
	return rec
}

func (f *reviewedMatchFixture) assertUnchanged(t *testing.T, status models.DownloadState, withPath bool) {
	t.Helper()
	ctx := context.Background()
	got, err := f.downloads.GetByID(ctx, f.dl.ID)
	if err != nil || got == nil || got.BookID == nil || *got.BookID != f.book.ID ||
		got.VariantID == nil || *got.VariantID != f.variant.ID || got.Status != status ||
		got.ImportRetryCount != db.ImportRetryLimit || got.ErrorMessage != "wrong work" ||
		got.TorrentID == nil || *got.TorrentID != "abcdef1234567890abcdef1234567890abcdef12" ||
		got.DownloadClientID == nil || *got.DownloadClientID != *f.dl.DownloadClientID {
		t.Fatalf("reviewed download changed: %+v %v", got, err)
	}
	if withPath && got.ImportPath != f.source || !withPath && got.ImportPath != "" {
		t.Fatalf("source path changed to %q", got.ImportPath)
	}
	rows, err := f.books.VariantRepo().ListByBook(ctx, f.book.ID)
	if err != nil || len(rows) != 1 || rows[0].Status != models.VariantReview {
		t.Fatalf("review variant changed: %+v %v", rows, err)
	}
	if rejected, err := f.books.RejectionRepo().IsRejected(ctx, f.book.ID, f.variant.ID, f.source); err != nil || !rejected {
		t.Fatalf("rejection was lost: %t %v", rejected, err)
	}
	if accepted, found, err := f.downloads.ResetImportRetry(ctx, f.dl.ID); err != nil || accepted || !found {
		t.Fatalf("generic retry bypassed review: accepted=%t found=%t err=%v", accepted, found, err)
	}
	f.scanner.importMu.Lock()
	calls := f.scanner.importCalls
	f.scanner.importMu.Unlock()
	if calls != 0 || f.clientHits.Load() != 0 {
		t.Fatalf("unsafe import/client called: scanner=%d client=%d", calls, f.clientHits.Load())
	}
	if data, err := os.ReadFile(f.source); err != nil || string(data) != "source unchanged" {
		t.Fatalf("seeded source changed: %q %v", data, err)
	}
}

// The first cross-book Match must fail: once it clears variant_id, a second
// Match back to the original book and generic retry can bypass source review.
// Same-book Match must also fail because it can import the reviewed source
// directly through the legacy scanner path.
func TestMatchDownloadRefusesReviewedVariantForSameAndForeignBook(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    models.DownloadState
		ownerMode string
		withPath  bool
		crossBook bool
	}{
		{"blocked foreign target with recorded path", models.StateImportBlocked, "foreign", true, true},
		{"blocked same book with recorded path", models.StateImportBlocked, "own", true, false},
		{"failed foreign target with no path", models.StateImportFailed, "own", false, true},
		{"failed same book without owner", models.StateImportFailed, "absent", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewedMatchFixture(t, tc.status, tc.ownerMode, tc.withPath)
			target := f.book.ID
			if tc.crossBook {
				target = f.other.ID
			}
			f.match(t, target)
			f.assertUnchanged(t, tc.status, tc.withPath)
			if tc.crossBook {
				f.match(t, f.book.ID) // returning to the original book must not be a second way around review
				f.assertUnchanged(t, tc.status, tc.withPath)
			}
		})
	}
}

func TestMatchDownloadReviewedVariantRejectsMalformedAndMissingIDsWithoutPathExposure(t *testing.T) {
	f := newReviewedMatchFixture(t, models.StateImportBlocked, "own", true)
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"negative download", fmt.Sprintf(`{"downloadId":-1,"bookId":%d}`, f.other.ID), http.StatusBadRequest},
		{"negative book", fmt.Sprintf(`{"downloadId":%d,"bookId":-1}`, f.dl.ID), http.StatusBadRequest},
		{"string id", fmt.Sprintf(`{"downloadId":"%d","bookId":%d}`, f.dl.ID, f.other.ID), http.StatusBadRequest},
		{"missing download", fmt.Sprintf(`{"downloadId":999999,"bookId":%d}`, f.other.ID), http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			f.h.MatchDownload(rec, httptest.NewRequest(http.MethodPost, "/api/v1/queue/manual-import/match", strings.NewReader(tc.body)))
			if rec.Code != tc.status || strings.Contains(rec.Body.String(), f.source) || strings.Contains(rec.Body.String(), f.clientURL) {
				t.Fatalf("invalid Match = %d %q, want %d without private path", rec.Code, rec.Body.String(), tc.status)
			}
			f.assertUnchanged(t, models.StateImportBlocked, true)
		})
	}
}
