package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

func TestVariantRetryExistingTorrentNamedAdminRoute(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newRequestsFixture(t, wellsRequestStub(), &fakeAdder{})
	ctx := context.Background()
	author := seedWells(t, f.authors)
	own := &models.Book{ForeignID: "OL-RETRY-OLD", AuthorID: author.ID, OwnerUserID: f.requester.ID, Title: "Retry Old", SortTitle: "retry old", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	other := &models.Book{ForeignID: "OL-RETRY-FOREIGN", AuthorID: author.ID, OwnerUserID: f.other.ID, Title: "Foreign", SortTitle: "foreign", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	for _, book := range []*models.Book{own, other} {
		if err := f.books.Create(ctx, book); err != nil {
			t.Fatal(err)
		}
	}
	ownCopy, err := f.books.VariantRepo().EnsureWanted(ctx, own.ID, "ebook", []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	foreignCopy, err := f.books.VariantRepo().EnsureWanted(ctx, other.ID, "ebook", []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	client := &models.DownloadClient{Name: "test-offline", Type: "qbittorrent", Host: "127.0.0.1", Port: 1}
	if err := db.NewDownloadClientRepo(f.database).Create(ctx, client); err != nil {
		t.Fatal(err)
	}
	const hash = "abcdef1234567890abcdef1234567890abcdef12"
	downloads := db.NewDownloadRepo(f.database)
	old := &models.Download{GUID: "retry-old-torrent", BookID: &own.ID, VariantID: &ownCopy[0].ID,
		Title: "Retry Old [ENG / EPUB]", Protocol: "torrent", DownloadClientID: &client.ID, Status: models.StateGrabbed}
	if err := downloads.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := downloads.SetTorrentID(ctx, old.ID, hash); err != nil {
		t.Fatal(err)
	}
	if err := downloads.UpdateStatus(ctx, old.ID, models.StateDownloading); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.books.VariantRepo().MarkDownloading(ctx, ownCopy[0].ID); err != nil || !ok {
		t.Fatalf("mark downloaded: %t %v", ok, err)
	}
	for _, status := range []models.DownloadState{models.StateCompleted, models.StateImportPending, models.StateImporting} {
		if err := downloads.UpdateStatus(ctx, old.ID, status); err != nil {
			t.Fatal(err)
		}
	}
	if err := downloads.SetErrorWithStatus(ctx, old.ID, models.StateImportBlocked, "file list unavailable"); err != nil {
		t.Fatal(err)
	}
	foreign := &models.Download{GUID: "retry-foreign-torrent", BookID: &other.ID, VariantID: &foreignCopy[0].ID,
		Title: "Foreign [ENG / EPUB]", Protocol: "torrent", Status: models.StateFailed}
	if err := downloads.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	h := NewBookHandler(f.books, nil, db.NewHistoryRepo(f.database), newMockBookSearcher()).WithDownloads(downloads)
	h.rejectionTorrentReader = func(*models.DownloadClient) rejectionTorrentReader {
		t.Fatal("existing-torrent review retry contacted qBittorrent")
		return nil
	}
	router := chi.NewRouter()
	router.Route("/api/v1", func(r chi.Router) { RegisterVariantRejectionRoute(r, h) })
	post := func(user *db.User, bookID, variantID, downloadID int64, body string) *httptest.ResponseRecorder {
		t.Helper()
		url := fmt.Sprintf("/api/v1/book/%d/variants/%d/retry-import", bookID, variantID)
		if downloadID > 0 {
			body = strings.ReplaceAll(body, `"downloadId":0`, fmt.Sprintf(`"downloadId":%d`, downloadID))
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
		if user != nil {
			req = asUser(req, user)
		} else {
			req = req.WithContext(auth.WithUserRole(req.Context(), auth.RoleAdmin))
		}
		router.ServeHTTP(rec, req)
		return rec
	}
	valid := `{"downloadId":0,"reason":"qBittorrent Files list recovered"}`
	for _, tc := range []struct {
		name                          string
		user                          *db.User
		bookID, variantID, downloadID int64
		body                          string
		code                          int
	}{
		{"non-admin", f.requester, own.ID, ownCopy[0].ID, old.ID, valid, http.StatusForbidden},
		{"unnamed admin", nil, own.ID, ownCopy[0].ID, old.ID, valid, http.StatusForbidden},
		{"foreign variant", f.admin, own.ID, foreignCopy[0].ID, old.ID, valid, http.StatusNotFound},
		{"foreign download", f.admin, own.ID, ownCopy[0].ID, foreign.ID, valid, http.StatusNotFound},
		{"invalid body", f.admin, own.ID, ownCopy[0].ID, 0, `{"downloadId":0,"reason":"try"}`, http.StatusBadRequest},
		{"blank reason", f.admin, own.ID, ownCopy[0].ID, old.ID, `{"downloadId":0,"reason":" "}`, http.StatusBadRequest},
		{"unknown field", f.admin, own.ID, ownCopy[0].ID, old.ID, `{"downloadId":0,"reason":"try","force":true}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := post(tc.user, tc.bookID, tc.variantID, tc.downloadID, tc.body); rec.Code != tc.code {
				t.Fatalf("retry = %d %s, want %d", rec.Code, rec.Body.String(), tc.code)
			}
		})
	}
	// The ordinary Queue Retry import is not a named admin review action.
	// Its refusal must tell the operator which distinct action to use.
	queueHandler := NewQueueHandler(downloads, db.NewDownloadClientRepo(f.database), f.books, db.NewHistoryRepo(f.database))
	queueReq := withURLParam(httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/queue/%d/retry-import", old.ID), nil), "id", fmt.Sprint(old.ID))
	queueRec := httptest.NewRecorder()
	queueHandler.RetryImport(queueRec, asUser(queueReq, f.admin))
	if queueRec.Code != http.StatusConflict || !strings.Contains(queueRec.Body.String(), "named admin") || !strings.Contains(queueRec.Body.String(), fmt.Sprintf("/book/%d/variants/%d/retry-import", own.ID, ownCopy[0].ID)) {
		t.Fatalf("generic retry did not explain reviewed-torrent action: %d %s", queueRec.Code, queueRec.Body.String())
	}
	if rec := post(f.admin, own.ID, ownCopy[0].ID, old.ID, valid); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"downloading"`) {
		t.Fatalf("named existing-torrent retry = %d %s", rec.Code, rec.Body.String())
	}
	row, err := downloads.GetByID(ctx, old.ID)
	if err != nil || row == nil || row.Status != models.StateImportFailed || row.TorrentID == nil || *row.TorrentID != hash || row.DownloadClientID == nil || *row.DownloadClientID != client.ID {
		t.Fatalf("retry changed old torrent identity: %+v %v", row, err)
	}
	copies, err := f.books.VariantRepo().ListByBook(ctx, own.ID)
	if err != nil || len(copies) != 1 || copies[0].Status != models.VariantDownloading {
		t.Fatalf("retry exposed wanted or left review: %+v %v", copies, err)
	}
	if rec := post(f.admin, own.ID, ownCopy[0].ID, old.ID, valid); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate retry = %d %s, want 409", rec.Code, rec.Body.String())
	}
	events, err := db.NewHistoryRepo(f.database).ListByBook(ctx, own.ID)
	if err != nil || len(events) != 1 || events[0].EventType != "variantImportRetried" || !strings.Contains(events[0].Data, `"actorUserId":`) {
		t.Fatalf("named audit missing or duplicated: %+v %v", events, err)
	}
}
