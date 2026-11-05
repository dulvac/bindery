package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/indexer"
	"github.com/vavallee/bindery/internal/indexer/newznab"
	"github.com/vavallee/bindery/internal/models"
)

// A search scoped to the Romanian variant must not advertise an English MAM
// release as a normal result, even if an indexer searcher returns both.
func TestMAMLanguageInteractiveSearchUsesRequestedVariant(t *testing.T) {
	f := newGrabSecFixture(t)
	book := regrabOwnedBook(t, f.database, f.books, "mam-languages", f.bob)
	if _, err := f.books.VariantRepo().EnsureWanted(f.ctx, book.ID, models.MediaTypeEbook, []string{"eng", "rum"}); err != nil {
		t.Fatal(err)
	}
	if err := db.NewSettingsRepo(f.database).Set(f.ctx, "search.preferredLanguage", "en"); err != nil {
		t.Fatal(err)
	}
	s := droppedSearcher{results: map[string][]newznab.SearchResult{models.MediaTypeEbook: {
		{GUID: "en", Title: "Some Book [ENG / EPUB]", NZBURL: "https://x/en?apikey=SECRET"},
		{GUID: "unknown", Title: "Some Book.epub", Language: "en", NZBURL: "https://x/unknown"},
		{GUID: "ro", Title: "Some Book [ROM / EPUB]", NZBURL: "https://x/ro"},
	}}}
	h := NewIndexerHandler(db.NewIndexerRepo(f.database), f.books, db.NewAuthorRepo(f.database),
		nil, s, db.NewSettingsRepo(f.database), db.NewBlocklistRepo(f.database)).WithSearchResults(f.queue.searchResults)
	rec := httptest.NewRecorder()
	req := withURLParam(httptest.NewRequest(http.MethodGet, "/indexer/book/1/search?language=rum", nil), "id", strconv.FormatInt(book.ID, 10))
	h.SearchBook(rec, asRole(req, f.bob, auth.RoleUser))
	if rec.Code != http.StatusOK {
		t.Fatalf("search status = %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Results []struct {
			GUID     string `json:"guid"`
			Language string `json:"language"`
		} `json:"results"`
		Filtered []struct {
			GUID      string `json:"guid"`
			Approved  bool   `json:"approved"`
			Rejection string `json:"rejection"`
		} `json:"filtered"`
		Debug struct {
			Query struct {
				RequestedLanguage string `json:"requestedLanguage"`
			} `json:"query"`
			Filters []struct {
				Stage, Reason string
			} `json:"filters"`
		} `json:"debug"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 2 || response.Results[0].GUID != "ro" || response.Results[1].GUID != "unknown" || response.Results[1].Language != "" {
		t.Errorf("results = %+v; want Romanian first, unknown second, not labelled English", response.Results)
	}
	if len(response.Filtered) != 1 || response.Filtered[0].GUID != "en" || response.Filtered[0].Approved || !strings.Contains(response.Filtered[0].Rejection, "rum") {
		t.Errorf("filtered = %+v; want explicit English tagged as wrong for Romanian", response.Filtered)
	}
	if response.Debug.Query.RequestedLanguage != "rum" {
		t.Errorf("debug request language = %q; want rum", response.Debug.Query.RequestedLanguage)
	}
	found := false
	for _, reason := range response.Debug.Filters {
		if reason.Stage == indexer.StageLanguage && strings.Contains(reason.Reason, "rum") && strings.Contains(reason.Reason, "eng") {
			found = true
		}
	}
	if !found || strings.Contains(rec.Body.String(), "SECRET") {
		t.Errorf("debug reasons missing or response leaked a credential: %s", rec.Body.String())
	}
}

// Without a query parameter, manual search chooses a wanted copy instead of
// an already imported English copy of the same format.
func TestMAMLanguageInteractiveSearchDefaultsToWantedVariant(t *testing.T) {
	f := newGrabSecFixture(t)
	book := regrabOwnedBook(t, f.database, f.books, "mam-wanted-language", f.bob)
	variants, err := f.books.VariantRepo().EnsureWanted(f.ctx, book.ID, models.MediaTypeEbook, []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.books.VariantRepo().SetStatus(f.ctx, variants[0].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	s := droppedSearcher{results: map[string][]newznab.SearchResult{models.MediaTypeEbook: {
		{GUID: "en", Title: "Some Book [ENG / EPUB]"},
		{GUID: "ro", Title: "Some Book [ROM / EPUB]"},
	}}}
	h := NewIndexerHandler(db.NewIndexerRepo(f.database), f.books, db.NewAuthorRepo(f.database),
		nil, s, db.NewSettingsRepo(f.database), db.NewBlocklistRepo(f.database)).WithSearchResults(f.queue.searchResults)
	rec := httptest.NewRecorder()
	req := withURLParam(httptest.NewRequest(http.MethodGet, "/indexer/book/1/search", nil), "id", strconv.FormatInt(book.ID, 10))
	h.SearchBook(rec, asRole(req, f.bob, auth.RoleUser))
	if rec.Code != http.StatusOK {
		t.Fatalf("search status = %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Results []struct {
			GUID string `json:"guid"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].GUID != "ro" {
		t.Errorf("wanted Romanian search results = %+v; want Romanian only", response.Results)
	}
}

// A broad legacy language filter must not erase the more useful per-release
// reason that a Romanian MAM marker disagrees with the English request.
func TestMAMLanguageInteractiveDebugExplainsWrongVariantBeforeProfile(t *testing.T) {
	f := newGrabSecFixture(t)
	book := regrabOwnedBook(t, f.database, f.books, "mam-debug-reason", f.bob)
	if _, err := f.books.VariantRepo().EnsureWanted(f.ctx, book.ID, models.MediaTypeEbook, []string{"eng"}); err != nil {
		t.Fatal(err)
	}
	if err := db.NewSettingsRepo(f.database).Set(f.ctx, "search.preferredLanguage", "en"); err != nil {
		t.Fatal(err)
	}
	s := droppedSearcher{results: map[string][]newznab.SearchResult{models.MediaTypeEbook: {
		{GUID: "wrong", Title: "Some Book [ROM / EPUB]", NZBURL: "https://x/ro"},
	}}}
	h := NewIndexerHandler(db.NewIndexerRepo(f.database), f.books, db.NewAuthorRepo(f.database),
		nil, s, db.NewSettingsRepo(f.database), db.NewBlocklistRepo(f.database)).WithSearchResults(f.queue.searchResults)
	rec := httptest.NewRecorder()
	req := withURLParam(httptest.NewRequest(http.MethodGet, "/indexer/book/1/search", nil), "id", strconv.FormatInt(book.ID, 10))
	h.SearchBook(rec, asRole(req, f.bob, auth.RoleUser))
	if rec.Code != http.StatusOK {
		t.Fatalf("search status = %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Filtered []struct {
			Rejection string `json:"rejection"`
		} `json:"filtered"`
		Debug struct {
			Filters []struct {
				Stage, Reason string
			} `json:"filters"`
		} `json:"debug"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Filtered) != 1 || !strings.Contains(response.Filtered[0].Rejection, "release language rum") || !strings.Contains(response.Filtered[0].Rejection, "variant eng") {
		t.Errorf("filtered reason = %+v; want specific mismatch", response.Filtered)
	}
	found := false
	for _, item := range response.Debug.Filters {
		if item.Stage == indexer.StageLanguage && strings.Contains(item.Reason, "release language rum") && strings.Contains(item.Reason, "variant eng") {
			found = true
		}
	}
	if !found {
		t.Errorf("interactive debug omitted the variant mismatch: %+v", response.Debug.Filters)
	}
}

func TestMAMLanguageInteractiveSearchRejectsUnrequestedVariant(t *testing.T) {
	f := newGrabSecFixture(t)
	book := regrabOwnedBook(t, f.database, f.books, "mam-invalid-language", f.bob)
	if _, err := f.books.VariantRepo().EnsureWanted(f.ctx, book.ID, models.MediaTypeEbook, []string{"rum"}); err != nil {
		t.Fatal(err)
	}
	h := NewIndexerHandler(db.NewIndexerRepo(f.database), f.books, db.NewAuthorRepo(f.database),
		nil, droppedSearcher{}, db.NewSettingsRepo(f.database), db.NewBlocklistRepo(f.database)).WithSearchResults(f.queue.searchResults)
	rec := httptest.NewRecorder()
	req := withURLParam(httptest.NewRequest(http.MethodGet, "/indexer/book/1/search?language=eng", nil), "id", strconv.FormatInt(book.ID, 10))
	h.SearchBook(rec, asRole(req, f.bob, auth.RoleUser))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("search of unrequested English variant = %d: %s", rec.Code, rec.Body.String())
	}
}
