package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

type wantedVariantRow struct {
	ID       int64             `json:"id"`
	Title    string            `json:"title"`
	Status   string            `json:"status"`
	Variants []variantResponse `json:"variants"`
}

type wantedVariantPage struct {
	Items  []wantedVariantRow `json:"items"`
	Total  int                `json:"total"`
	Limit  int                `json:"limit"`
	Offset int                `json:"offset"`
}

func TestListWanted_VariantMissingAfterEnglishImport(t *testing.T) {
	h, books, _, author, ctx := bookFixture(t)
	variants := books.VariantRepo()
	create := func(title, status string, monitored, excluded bool) *models.Book {
		t.Helper()
		book := &models.Book{ForeignID: "wanted-variant-" + title, AuthorID: author.ID,
			Title: title, SortTitle: title, Status: status, Monitored: monitored,
			MediaType: models.MediaTypeEbook, MetadataProvider: "openlibrary"}
		if err := books.Create(ctx, book); err != nil {
			t.Fatal(err)
		}
		if excluded {
			if err := books.SetExcluded(ctx, book.ID, true); err != nil {
				t.Fatal(err)
			}
		}
		return book
	}
	seedCopies := func(book *models.Book, languages ...string) []models.BookVariant {
		t.Helper()
		copies, err := variants.EnsureWanted(ctx, book.ID, models.MediaTypeEbook, languages)
		if err != nil {
			t.Fatal(err)
		}
		return copies
	}
	dune := create("Dune", models.BookStatusWanted, true, false)
	copies := seedCopies(dune, "eng", "rum", "fre")
	if err := books.TrackVariantFile(ctx, dune.ID, copies[0].ID, "ebook", "/library/Dune-eng.epub", "eng", "verified"); err != nil {
		t.Fatal(err)
	}
	create("Legacy", models.BookStatusWanted, true, false)
	allImported := create("All Imported", models.BookStatusImported, true, false)
	for _, v := range seedCopies(allImported, "eng", "rum") {
		if err := variants.SetStatus(ctx, v.ID, models.VariantImported); err != nil {
			t.Fatal(err)
		}
	}
	review := create("Review Only", models.BookStatusWanted, true, false)
	for _, v := range seedCopies(review, "eng") {
		if err := variants.SetStatus(ctx, v.ID, models.VariantReview); err != nil {
			t.Fatal(err)
		}
	}
	seedCopies(create("Unmonitored", models.BookStatusImported, false, false), "rum")
	seedCopies(create("Excluded", models.BookStatusImported, true, true), "rum")

	get := func(query string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ListWanted(rec, httptest.NewRequest(http.MethodGet, "/api/v1/wanted/missing"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", query, rec.Code, rec.Body.String())
		}
		return rec
	}
	page := func(query string) wantedVariantPage {
		t.Helper()
		var p wantedVariantPage
		if err := json.Unmarshal(get(query).Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	got := page("?limit=1&sort=title-za")
	if got.Total != 2 || got.Limit != 1 || got.Offset != 0 || len(got.Items) != 1 || got.Items[0].Title != "Legacy" {
		t.Errorf("first page = %+v, want one Legacy of two wanted books", got)
	}
	got = page("?limit=1&offset=1&sort=title-za")
	if got.Total != 2 || len(got.Items) != 1 || got.Items[0].ID != dune.ID || got.Items[0].Status != models.BookStatusImported {
		t.Fatalf("second page = %+v, want imported Dune once", got)
	}
	if want := []variantResponse{
		{ID: copies[0].ID, MediaType: "ebook", Language: "eng", Status: "imported", Verification: "verified"},
		{ID: copies[1].ID, MediaType: "ebook", Language: "fre", Status: "wanted"},
		{ID: copies[2].ID, MediaType: "ebook", Language: "rum", Status: "wanted"},
	}; !slices.Equal(got.Items[0].Variants, want) {
		t.Errorf("Dune variants = %+v, want real IDs, statuses and languages %+v", got.Items[0].Variants, want)
	}
	var raw struct {
		Items []struct {
			Variants []map[string]any `json:"variants"`
		} `json:"items"`
	}
	if err := json.Unmarshal(get("?limit=1&search=Dune").Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, v := range raw.Items[0].Variants {
		if len(v) != 5 { // only id, mediaType, language, status and verification
			t.Errorf("wanted variant contains more than safe status fields: %+v", v)
		}
	}
	got = page("?limit=1&search=dune")
	if got.Total != 1 || len(got.Items) != 1 || got.Items[0].ID != dune.ID {
		t.Errorf("searched page = %+v, want Dune once", got)
	}
	got = page("?limit=20&includeExcluded=true")
	if got.Total != 3 {
		t.Errorf("including excluded = %+v, want Legacy, Dune and Excluded", got)
	}
	var bare []wantedVariantRow
	if err := json.Unmarshal(get("").Body.Bytes(), &bare); err != nil {
		t.Fatalf("bare array: %v", err)
	}
	if len(bare) != 2 || bare[0].ID != dune.ID || bare[1].Title != "Legacy" || !slices.Equal(bare[0].Variants, []variantResponse{
		{ID: copies[0].ID, MediaType: "ebook", Language: "eng", Status: "imported", Verification: "verified"},
		{ID: copies[1].ID, MediaType: "ebook", Language: "fre", Status: "wanted"},
		{ID: copies[2].ID, MediaType: "ebook", Language: "rum", Status: "wanted"},
	}) {
		t.Errorf("bare wanted list = %+v, want Dune and Legacy with real copy states", bare)
	}
}

func TestListWanted_VariantOwnerChangeDoesNotExposeStatus(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newListScopeFixture(t)
	ctx := context.Background()
	books := db.NewBookRepo(f.database)
	bob, err := books.GetByForeignID(ctx, "b-bob")
	if err != nil || bob == nil {
		t.Fatalf("Bob's book: %v, %v", bob, err)
	}
	copies, err := books.VariantRepo().EnsureWanted(ctx, bob.ID, "ebook", []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.ExecContext(ctx, "UPDATE books SET status='imported' WHERE id=?", bob.ID); err != nil {
		t.Fatal(err)
	}
	get := func() wantedVariantPage {
		t.Helper()
		rec := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/wanted/missing?limit=10", nil)
		request = request.WithContext(auth.WithUserRole(auth.WithUserID(request.Context(), f.bob), "user"))
		f.booksH.ListWanted(rec, request)
		if rec.Code != http.StatusOK {
			t.Fatalf("wanted: %d %s", rec.Code, rec.Body.String())
		}
		var page wantedVariantPage
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	before := get()
	if before.Total != 2 || len(before.Items) != 2 || before.Items[0].ID != bob.ID || !slices.Equal(before.Items[0].Variants, []variantResponse{
		{ID: copies[0].ID, MediaType: "ebook", Language: "rum", Status: "wanted"},
	}) {
		t.Fatalf("Bob before reassignment: %+v", before)
	}
	if _, err := f.database.ExecContext(ctx, "UPDATE books SET owner_user_id=? WHERE id=?", f.alice, bob.ID); err != nil {
		t.Fatal(err)
	}
	after := get()
	if after.Total != 1 || len(after.Items) != 1 || after.Items[0].Title != "Global Book" || len(after.Items[0].Variants) != 0 {
		t.Errorf("Bob after reassignment: %+v; wanted only unowned book, no copy leak", after)
	}
}
