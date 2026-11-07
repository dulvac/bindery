package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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

func TestListWanted_InactiveAudiobookNotCountedUntilFormatReenabled(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newListScopeFixture(t)
	ctx := context.Background()
	books := db.NewBookRepo(f.database)
	variants := books.VariantRepo()
	var bobID int64
	var audioID int64
	for _, tc := range []struct{ foreignID string }{{"b-bob"}, {"b-alice"}} {
		book, err := books.GetByForeignID(ctx, tc.foreignID)
		if err != nil || book == nil {
			t.Fatalf("book %s: %+v, %v", tc.foreignID, book, err)
		}
		book.MediaType = models.MediaTypeBoth
		if err := books.Update(ctx, book); err != nil {
			t.Fatal(err)
		}
		ebook, err := variants.EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
		if err != nil {
			t.Fatal(err)
		}
		audio, err := variants.EnsureWanted(ctx, book.ID, models.MediaTypeAudiobook, []string{"rum"})
		if err != nil {
			t.Fatal(err)
		}
		if err := books.TrackVariantFile(ctx, book.ID, ebook[0].ID, models.MediaTypeEbook, "/library/"+tc.foreignID+".epub", "eng", "verified"); err != nil {
			t.Fatal(err)
		}
		if tc.foreignID == "b-bob" {
			bobID, audioID = book.ID, audio[0].ID
		}
	}
	bobCtx := auth.WithUserRole(auth.WithUserID(ctx, f.bob), "user")
	get := func(query string) wantedVariantPage {
		t.Helper()
		rec := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/wanted/missing?limit=1"+query, nil).WithContext(bobCtx)
		f.booksH.ListWanted(rec, request)
		if rec.Code != http.StatusOK {
			t.Fatalf("wanted %s: %d %s", query, rec.Code, rec.Body.String())
		}
		var page wantedVariantPage
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	getBare := func() []wantedVariantRow {
		t.Helper()
		rec := httptest.NewRecorder()
		f.booksH.ListWanted(rec, httptest.NewRequest(http.MethodGet, "/api/v1/wanted/missing", nil).WithContext(bobCtx))
		if rec.Code != http.StatusOK {
			t.Fatalf("bare wanted: %d %s", rec.Code, rec.Body.String())
		}
		var rows []wantedVariantRow
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	check := func(name string, wantBob bool) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			wantTotal, wantAudio := 1, 0 // the unowned, legacy ebook always qualifies
			if wantBob {
				wantTotal, wantAudio = 2, 1
			}
			all := get("")
			if all.Total != wantTotal || len(all.Items) != 1 || (all.Items[0].ID == bobID) != wantBob {
				t.Errorf("Bob's first page = %+v, want total %d with Bob present %v", all, wantTotal, wantBob)
			}
			for _, tc := range []struct {
				query string
				want  int
			}{
				{"&mediaType=ebook", 1},
				{"&mediaType=audiobook", wantAudio},
				{"&mediaType=both", wantAudio},
				{"&search=Bob", wantAudio},
			} {
				got := get(tc.query)
				if got.Total != tc.want || len(got.Items) != tc.want {
					t.Errorf("%s: %+v, want total %d", tc.query, got, tc.want)
				} else if tc.want == 1 {
					if tc.query == "&mediaType=ebook" && got.Items[0].Title != "Global Book" ||
						tc.query != "&mediaType=ebook" && got.Items[0].ID != bobID {
						t.Errorf("%s: %+v, want only the matching scoped book", tc.query, got)
					}
				}
			}
			bare := getBare()
			if len(bare) != wantTotal || (len(bare) > 1 && bare[0].ID != bobID) || len(bare) == 0 || bare[len(bare)-1].Title != "Global Book" {
				t.Errorf("bare Bob scope = %+v, want Bob (if enabled) and Global, never Alice", bare)
			}
			if wantBob {
				if len(all.Items) == 0 || all.Items[0].ID != bobID || !slices.Contains(all.Items[0].Variants, variantResponse{ID: audioID, MediaType: "audiobook", Language: "rum", Status: "wanted"}) {
					t.Errorf("Bob's wanted audio status = %+v, want kept real variant ID %d", all.Items, audioID)
				}
			}
		})
	}
	updateBob := func(mediaType string) {
		t.Helper()
		id := strconv.FormatInt(bobID, 10)
		body, err := json.Marshal(map[string]string{"mediaType": mediaType})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPut, "/api/v1/book/"+id, bytes.NewReader(body)).WithContext(bobCtx)
		rec := httptest.NewRecorder()
		f.booksH.Update(rec, withURLParam(request, "id", id))
		if rec.Code != http.StatusOK {
			t.Fatalf("format %s: %d %s", mediaType, rec.Code, rec.Body.String())
		}
	}

	check("both", true)
	updateBob(models.MediaTypeEbook)
	check("ebook-only", false)
	kept, err := variants.ListByBook(ctx, bobID)
	if err != nil || !slices.ContainsFunc(kept, func(v models.BookVariant) bool { return v.ID == audioID && v.Status == models.VariantWanted }) {
		t.Errorf("narrowing must keep Romanian audiobook for later: %+v, %v", kept, err)
	}
	updateBob(models.MediaTypeBoth)
	check("both-again", true)
}
