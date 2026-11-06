package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/indexer/newznab"
	"github.com/vavallee/bindery/internal/models"
)

func TestInteractiveSearchFormatAndLanguageStayOnOneWantedVariant(t *testing.T) {
	f := newGrabSecFixture(t)
	book := regrabOwnedBook(t, f.database, f.books, "mam-scoped-format", f.bob)
	if _, err := f.database.ExecContext(f.ctx, "UPDATE books SET media_type=? WHERE id=?", models.MediaTypeBoth, book.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.books.VariantRepo().EnsureWanted(f.ctx, book.ID, models.MediaTypeEbook, []string{"rum"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.books.VariantRepo().EnsureWanted(f.ctx, book.ID, models.MediaTypeAudiobook, []string{"eng"}); err != nil {
		t.Fatal(err)
	}
	s := &mockIndexerSearcher{ebookResults: []newznab.SearchResult{{GUID: "ebook-ro", Title: "Dune [ROM / EPUB]"}}, audioResults: []newznab.SearchResult{{GUID: "audio-en", Title: "Dune [ENG / M4B]"}}}
	h := NewIndexerHandler(db.NewIndexerRepo(f.database), f.books, db.NewAuthorRepo(f.database), nil, s, db.NewSettingsRepo(f.database), db.NewBlocklistRepo(f.database))
	search := func(query string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := withURLParam(httptest.NewRequest(http.MethodPost, "/book/1/search?"+query, nil), "id", strconv.FormatInt(book.ID, 10))
		h.SearchBook(rec, asRole(req, f.bob, auth.RoleUser))
		return rec
	}
	rec := search("mediaType=ebook&language=rum")
	if rec.Code != http.StatusOK {
		t.Fatalf("ebook search = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Results  []struct{ GUID, MediaType string }
		Filtered []struct{ GUID string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].GUID != "ebook-ro" || out.Results[0].MediaType != "ebook" || len(out.Filtered) != 0 {
		t.Errorf("scoped results = %+v, filtered = %+v", out.Results, out.Filtered)
	}
	if got := s.criteriaFor(models.MediaTypeAudiobook); got.MediaType != "" {
		t.Errorf("audio search ran: %+v", got)
	}
	for _, query := range []string{"mediaType=audiobook&language=rum", "mediaType=both&language=rum", "mediaType=ebook&language=fre", "mediaType=ebook"} {
		if rec := search(query); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d; want 400", query, rec.Code)
		}
	}
}
