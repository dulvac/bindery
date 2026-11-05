package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/httpsec"
	"github.com/vavallee/bindery/internal/indexer/newznab"
	"github.com/vavallee/bindery/internal/models"
)

func queueVariantGrab(t *testing.T, h *QueueHandler, ctx context.Context, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.Grab(rec, httptest.NewRequest(http.MethodPost, "/api/v1/queue/grab", bytes.NewReader(body)).WithContext(ctx))
	return rec
}

func TestQueueVariantGrabAnotherUsersVariantIs404(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newGrabSecFixture(t)
	own := regrabOwnedBook(t, f.database, f.books, "variant-owned-bob", f.bob)
	other := regrabOwnedBook(t, f.database, f.books, "variant-owned-alice", f.alice)
	variants, err := f.books.VariantRepo().EnsureWanted(f.ctx, other.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	f.queue.searchResults.remember([]newznab.SearchResult{{GUID: f.guid, Title: "Lee Child - One Shot [ENG / EPUB]", NZBURL: f.downloadURL, Protocol: "usenet"}})
	payload := map[string]any{"guid": f.guid, "title": "Lee Child - One Shot [ENG / EPUB]", "nzbUrl": f.downloadURL,
		"bookId": own.ID, "variantId": variants[0].ID, "mediaType": models.MediaTypeEbook, "protocol": "usenet"}
	rec := queueVariantGrab(t, f.queue, auth.WithUserRole(auth.WithUserID(f.ctx, f.bob), auth.RoleUser), payload)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("other user's variant = %d: %s; want 404", rec.Code, rec.Body.String())
	}
	if f.adds.Load() != 0 {
		t.Fatal("unauthorized variant sent to downloader")
	}
}

// A failed same-user download can lend its book to a re-grab. An explicit
// bookId:0 must not turn that inherited variant book into an unbound send.
func TestQueueVariantGrabZeroBookIDCannotRegrabWithoutVariant(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newGrabSecFixture(t)
	book := regrabOwnedBook(t, f.database, f.books, "zero-id-retry", f.bob)
	book.MediaType = models.MediaTypeEbook
	if err := f.books.Update(f.ctx, book); err != nil {
		t.Fatal(err)
	}
	copies, err := f.books.VariantRepo().EnsureWanted(f.ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	old := &models.Download{GUID: f.guid, BookID: &book.ID, VariantID: &copies[0].ID,
		OwnerUserID: f.bob, Title: "Lee Child - One Shot [ENG / EPUB]", NZBURL: f.downloadURL,
		Status: models.StateFailed, Protocol: "usenet"}
	if err := f.downloads.Create(f.ctx, old); err != nil {
		t.Fatal(err)
	}
	f.queue.searchResults.remember([]newznab.SearchResult{{GUID: f.guid, Title: old.Title,
		NZBURL: f.downloadURL, Protocol: "usenet"}})
	payload := map[string]any{"guid": f.guid, "title": old.Title, "nzbUrl": f.downloadURL,
		"bookId": 0, "mediaType": models.MediaTypeEbook, "protocol": "usenet"}
	rec := queueVariantGrab(t, f.queue, auth.WithUserRole(auth.WithUserID(f.ctx, f.bob), auth.RoleUser), payload)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("zero bookId retry without variant = %d: %s; want 400", rec.Code, rec.Body.String())
	}
	if f.adds.Load() != 0 {
		t.Fatal("retry without a variant reached the download client")
	}
	got, err := f.downloads.GetByID(f.ctx, old.ID)
	if err != nil || got == nil || got.Status != models.StateFailed || got.VariantID == nil || *got.VariantID != copies[0].ID {
		t.Fatalf("refused retry changed existing row: %+v, %v", got, err)
	}
}

func TestQueueVariantGrabWrongLanguageAndMissingSelectionRefused(t *testing.T) {
	defer httpsec.AllowLoopbackForTests()()
	rec, srv := newCategoryRecorder(t)
	h, database, book, ctx := categoryFixture(t, srv.URL)
	copies, err := db.NewBookVariantRepo(database).EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"guid": "manual-ro", "title": "Zogarth - The Primal Hunter 9 [ROM / EPUB]",
		"nzbUrl": srv.URL + "/manual.nzb", "bookId": book.ID, "protocol": "usenet", "mediaType": models.MediaTypeEbook}
	// A legacy call without a variant may still grab a legacy book, but a
	// variant book must not gain an unbound download that silently fills one.
	if got := queueVariantGrab(t, h, ctx, payload); got.Code != http.StatusBadRequest {
		t.Fatalf("variant book without selection = %d: %s", got.Code, got.Body.String())
	}
	payload["variantId"] = copies[0].ID
	if got := queueVariantGrab(t, h, ctx, payload); got.Code != http.StatusBadRequest {
		t.Fatalf("Romanian tagged release for English = %d: %s", got.Code, got.Body.String())
	}
	payload["mediaType"] = models.MediaTypeAudiobook
	if got := queueVariantGrab(t, h, ctx, payload); got.Code != http.StatusBadRequest {
		t.Fatalf("wrong format = %d: %s", got.Code, got.Body.String())
	}
	if len(rec.cats) != 0 {
		t.Errorf("unsafe manual grab dispatched: %+v", rec.cats)
	}
	payload["variantId"] = copies[1].ID
	payload["mediaType"] = models.MediaTypeEbook
	if got := queueVariantGrab(t, h, ctx, payload); got.Code != http.StatusAccepted {
		t.Fatalf("matching Romanian variant = %d: %s", got.Code, got.Body.String())
	}
	rows, err := h.downloads.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].VariantID == nil || *rows[0].VariantID != copies[1].ID || rows[0].Status != models.StateDownloading {
		t.Fatalf("manual download not associated with selected variant: %+v", rows)
	}
	current, err := db.NewBookVariantRepo(database).ListByBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current[0].Status != models.VariantWanted || current[1].Status != models.VariantDownloading {
		t.Fatalf("variant states = %+v", current)
	}
}

func TestQueueVariantGrabNamedAdminOverrideMustPersist(t *testing.T) {
	defer httpsec.AllowLoopbackForTests()()
	_, srv := newCategoryRecorder(t)
	h, database, book, ctx := categoryFixture(t, srv.URL)
	copies, err := db.NewBookVariantRepo(database).EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"guid": "admin-override", "title": "Zogarth - The Primal Hunter 9 [ROM / EPUB]",
		"nzbUrl": srv.URL + "/override.nzb", "bookId": book.ID, "variantId": copies[0].ID,
		"protocol": "usenet", "mediaType": models.MediaTypeEbook, "languageOverrideReason": "owner verified bilingual release"}
	if got := queueVariantGrab(t, h, ctx, payload); got.Code != http.StatusBadRequest {
		t.Fatalf("unnamed admin override = %d: %s", got.Code, got.Body.String())
	}
	registry := NewSearchResultRegistry()
	registry.remember([]newznab.SearchResult{{GUID: "admin-override", Title: "Zogarth - The Primal Hunter 9 [ROM / EPUB]",
		NZBURL: srv.URL + "/override.nzb", Protocol: "usenet"}})
	h.WithSearchResults(registry)
	uid, _ := regrabUsers(t, database)
	admin := auth.WithUserRole(auth.WithUserID(ctx, uid), auth.RoleAdmin)
	if got := queueVariantGrab(t, h, admin, payload); got.Code != http.StatusAccepted {
		t.Fatalf("named admin override = %d: %s", got.Code, got.Body.String())
	}
	rows, err := h.downloads.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].VariantID == nil || *rows[0].VariantID != copies[0].ID {
		t.Fatalf("override download association = %+v", rows)
	}
	// The decision must survive a process restart, and must name the actor,
	// affected variant and the specific reason, not just a transient flag.
	events, err := db.NewHistoryRepo(database).ListByBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if !strings.Contains(event.Data, "owner verified bilingual release") {
			continue
		}
		var audit struct {
			ActorUserID int64 `json:"actorUserId"`
			VariantID   int64 `json:"variantId"`
			DownloadID  int64 `json:"downloadId"`
		}
		if err := json.Unmarshal([]byte(event.Data), &audit); err != nil {
			t.Fatal(err)
		}
		if audit.ActorUserID != uid || audit.VariantID != copies[0].ID || audit.DownloadID != rows[0].ID {
			t.Fatalf("override lacks actor/variant/download identity: %+v", event)
		}
		found = true
	}
	if !found {
		t.Fatal("no persisted named override for this release")
	}
}
