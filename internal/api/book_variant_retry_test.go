package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

// A retry is a deliberate, audited database transition, not a resubmission
// of the rejected source or a change to any download-client state.
func TestVariantRetryAdminAuditAndNoDownloadMutation(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newRequestsFixture(t, wellsRequestStub(), &fakeAdder{})
	ctx := context.Background()
	author := seedWells(t, f.authors)
	own := &models.Book{ForeignID: "OL-RETRY", AuthorID: author.ID, OwnerUserID: f.requester.ID, Title: "Retry", SortTitle: "retry", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	other := &models.Book{ForeignID: "OL-OTHER", AuthorID: author.ID, OwnerUserID: f.other.ID, Title: "Other", SortTitle: "other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	for _, b := range []*models.Book{own, other} {
		if err := f.books.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	copies, err := f.books.VariantRepo().EnsureWanted(ctx, own.ID, "ebook", []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := f.books.VariantRepo().EnsureWanted(ctx, other.ID, "ebook", []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []models.BookVariant{copies[0], copies[1], stranger[0]} {
		if err := f.books.VariantRepo().SetStatus(ctx, v.ID, models.VariantReview); err != nil {
			t.Fatal(err)
		}
	}
	// The rejected download and torrent identity remain in the database;
	// failed is historical, not an active in-flight import.
	torrentHash := "torrent-shall-remain-seeding"
	downloads := db.NewDownloadRepo(f.database)
	old := &models.Download{GUID: "rejected", BookID: &own.ID, VariantID: &copies[0].ID,
		Title: "Rejected copy", NZBURL: "https://example.invalid/rejected", TorrentID: &torrentHash,
		Status: models.StateFailed, Protocol: "torrent"}
	if err := downloads.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	searcher := newMockBookSearcher()
	h := NewBookHandler(f.books, nil, db.NewHistoryRepo(f.database), searcher).WithDownloads(downloads)
	h.rejectionTorrentReader = func(*models.DownloadClient) rejectionTorrentReader {
		t.Fatal("review retry contacted a torrent client")
		return nil
	}
	router := chi.NewRouter()
	router.Route("/api/v1", func(r chi.Router) { RegisterVariantRejectionRoute(r, h) })
	post := func(user *db.User, bookID, variantID int64, body string) *httptest.ResponseRecorder {
		t.Helper()
		url := fmt.Sprintf("/api/v1/book/%d/variants/%d/retry", bookID, variantID)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, asUser(httptest.NewRequest(http.MethodPost, url, strings.NewReader(body)), user))
		return rec
	}
	if rec := post(f.requester, own.ID, copies[0].ID, `{"reason":"wrong release"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("requester retry = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if rec := post(f.admin, own.ID, stranger[0].ID, `{"reason":"wrong release"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("other book variant = %d %s, want 404", rec.Code, rec.Body.String())
	}
	if rec := post(f.requester, other.ID, stranger[0].ID, `{"reason":"wrong release"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("requester another book = %d %s, want 403", rec.Code, rec.Body.String())
	}
	for _, body := range []string{`{}`, `{"reason":"   "}`, `{"reason":"ok","downloadId":1}`} {
		if rec := post(f.admin, own.ID, copies[0].ID, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid reason/body %s = %d %s, want 400", body, rec.Code, rec.Body.String())
		}
	}
	if rec := post(f.admin, own.ID, copies[0].ID, `{"reason":"Dune biography, seek novel instead"}`); rec.Code != http.StatusOK {
		t.Fatalf("admin retry = %d %s, want 200", rec.Code, rec.Body.String())
	}
	variants, err := f.books.VariantRepo().ListByBook(ctx, own.ID)
	if err != nil || variants[0].Status != models.VariantWanted || variants[1].Status != models.VariantReview {
		t.Fatalf("retry altered the wrong variants: %+v %v", variants, err)
	}
	stored, err := downloads.GetByID(ctx, old.ID)
	if err != nil || stored.Status != models.StateFailed || stored.TorrentID == nil || *stored.TorrentID != torrentHash || stored.VariantID == nil || *stored.VariantID != copies[0].ID {
		t.Fatalf("retry altered the rejected torrent/download: %+v %v", stored, err)
	}
	searcher.assertNoCall(t, 10*time.Millisecond)
	events, err := db.NewHistoryRepo(f.database).ListByBook(ctx, own.ID)
	if err != nil || len(events) != 1 || events[0].EventType != "variantReviewRetried" {
		t.Fatalf("retry audit = %+v %v", events, err)
	}
	var audit struct {
		ActorUserID int64  `json:"actorUserId"`
		VariantID   int64  `json:"variantId"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(events[0].Data), &audit); err != nil {
		t.Fatal(err)
	}
	if audit.ActorUserID != f.admin.ID || audit.VariantID != copies[0].ID || audit.Reason != "Dune biography, seek novel instead" {
		t.Fatalf("wrong audit actor or reason: %+v", audit)
	}
	if rec := post(f.admin, own.ID, copies[0].ID, `{"reason":"repeat"}`); rec.Code != http.StatusConflict {
		t.Fatalf("idempotent refusal = %d %s, want 409", rec.Code, rec.Body.String())
	}
	events, err = db.NewHistoryRepo(f.database).ListByBook(ctx, own.ID)
	if err != nil || len(events) != 1 {
		t.Fatalf("repeat wrote audit: %+v %v", events, err)
	}
	// An imported file or a live import must block reset even if a stale
	// status row still says review.
	if _, err := f.database.ExecContext(ctx, `INSERT INTO book_files (book_id, format, path, variant_id, language, verification) VALUES (?, 'ebook', '/private/already-imported.epub', ?, 'rum', 'verified')`, own.ID, copies[1].ID); err != nil {
		t.Fatal(err)
	}
	if rec := post(f.admin, own.ID, copies[1].ID, `{"reason":"still has a file"}`); rec.Code != http.StatusConflict {
		t.Fatalf("linked import = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if _, err := f.database.ExecContext(ctx, `DELETE FROM book_files WHERE variant_id = ?`, copies[1].ID); err != nil {
		t.Fatal(err)
	}
	// A real grab reserves a wanted variant before a later check marks it
	// review. DownloadRepo correctly refuses creating one already in review.
	if err := f.books.VariantRepo().SetStatus(ctx, copies[1].ID, models.VariantWanted); err != nil {
		t.Fatal(err)
	}
	active := &models.Download{GUID: "in-flight", BookID: &own.ID, VariantID: &copies[1].ID, Title: "In flight", NZBURL: "https://example.invalid/live", Status: models.StateDownloading, Protocol: "torrent"}
	if err := downloads.Create(ctx, active); err != nil {
		t.Fatal(err)
	}
	if err := f.books.VariantRepo().SetStatus(ctx, copies[1].ID, models.VariantReview); err != nil {
		t.Fatal(err)
	}
	if rec := post(f.admin, own.ID, copies[1].ID, `{"reason":"still downloading"}`); rec.Code != http.StatusConflict {
		t.Fatalf("active download = %d %s, want 409", rec.Code, rec.Body.String())
	}
}
