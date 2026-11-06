package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"

	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/indexer/newznab"
	"github.com/vavallee/bindery/internal/models"
)

// The wire evidence comes from the server's title parser for both normal and
// filtered releases. An indexer-supplied language alone is not proof.
func TestInteractiveSearchReturnsCanonicalReleaseLanguages(t *testing.T) {
	f := newGrabSecFixture(t)
	book := regrabOwnedBook(t, f.database, f.books, "canonical-release-languages", f.bob)
	if _, err := f.books.VariantRepo().EnsureWanted(f.ctx, book.ID, models.MediaTypeEbook, []string{"rum"}); err != nil {
		t.Fatal(err)
	}
	s := &mockIndexerSearcher{ebookResults: []newznab.SearchResult{
		{GUID: "he", Title: "Dune [HE / EPUB]", Language: "ro"},
		{GUID: "heb", Title: "Dune [HEB / EPUB]"},
		{GUID: "web", Title: "Dune [WEB / EPUB]", Language: "he"},
		{GUID: "rom", Title: "Dune [ROM / EPUB]"},
	}}
	h := NewIndexerHandler(db.NewIndexerRepo(f.database), f.books, db.NewAuthorRepo(f.database), nil, s, db.NewSettingsRepo(f.database), db.NewBlocklistRepo(f.database))
	rec := httptest.NewRecorder()
	req := withURLParam(httptest.NewRequest(http.MethodPost, "/book/1/search?mediaType=ebook&language=rum", nil), "id", strconv.FormatInt(book.ID, 10))
	h.SearchBook(rec, asRole(req, f.bob, auth.RoleUser))
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Results, Filtered []struct {
			GUID             string   `json:"guid"`
			Language         string   `json:"language"`
			ReleaseLanguages []string `json:"releaseLanguages"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 2 || response.Results[0].GUID != "rom" || response.Results[1].GUID != "web" ||
		!slices.Equal(response.Results[0].ReleaseLanguages, []string{"rum"}) ||
		response.Results[1].ReleaseLanguages == nil || len(response.Results[1].ReleaseLanguages) != 0 || response.Results[1].Language != "" {
		t.Errorf("results = %+v; want unknown WEB [] (not indexer he) and known Romanian", response.Results)
	}
	if len(response.Filtered) != 2 || response.Filtered[0].GUID != "he" || response.Filtered[1].GUID != "heb" ||
		!slices.Equal(response.Filtered[0].ReleaseLanguages, []string{"heb"}) ||
		!slices.Equal(response.Filtered[1].ReleaseLanguages, []string{"heb"}) {
		t.Errorf("filtered = %+v; want server-known HE/HEB both tagged heb", response.Filtered)
	}
}

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
