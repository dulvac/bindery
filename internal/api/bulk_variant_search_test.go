package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

// A book-level imported path must not suppress a different wanted language.
// This fixture has an actual tracked English file, rather than a synthetic
// compatibility path, so the repository projects status=imported as it would
// after a real import.
func importedBookWithWantedRomanian(t *testing.T, books *db.BookRepo, authorID int64, ctx context.Context) *models.Book {
	t.Helper()
	book := mustCreateBook(t, books, ctx, &models.Book{
		ForeignID: "OL_VARIANT_BULK", AuthorID: authorID, Title: "Dune", SortTitle: "dune",
		Status: models.BookStatusWanted, MediaType: models.MediaTypeEbook, Monitored: true,
		MetadataProvider: "openlibrary", Genres: []string{},
	})
	copies, err := books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	if err := books.TrackVariantFile(ctx, book.ID, copies[0].ID, models.MediaTypeEbook, "/library/Dune-eng.epub", "eng", "verified"); err != nil {
		t.Fatal(err)
	}
	got, err := books.GetByID(ctx, book.ID)
	if err != nil || got == nil || got.Status != models.BookStatusImported || got.EbookFilePath == "" {
		t.Fatalf("expected imported English ebook with a Romanian copy still wanted: book=%+v err=%v", got, err)
	}
	return got
}

func TestBulkSearch_ImportedEnglishStillQueuesWantedRomanian(t *testing.T) {
	searchPaceInterval = 0
	t.Cleanup(func() { searchPaceInterval = 3 * time.Second })
	for _, route := range []string{"book", "wanted", "author"} {
		t.Run(route, func(t *testing.T) {
			searcher := newMockBookSearcher()
			h, _, books, author, ctx := bulkFixtureWithSearcher(t, searcher)
			book := importedBookWithWantedRomanian(t, books, author.ID, ctx)
			id, handler := book.ID, h.BooksBulk
			switch route {
			case "wanted":
				handler = h.WantedBulk
			case "author":
				id, handler = author.ID, h.AuthorsBulk
			}
			rec := postBulk(t, handler, fmt.Sprintf(`{"ids":[%d],"action":"search"}`, id))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			result := decodeBulk(t, rec).Results[strconv.FormatInt(id, 10)]
			if !result.OK || !result.Queued || result.SearchSkipped != "" {
				t.Fatalf("imported English, wanted Romanian: %+v; want ok and queued", result)
			}
			if got := searcher.waitForCall(t, time.Second); got.ID != book.ID {
				t.Fatalf("searched book %d, want %d", got.ID, book.ID)
			}
			searcher.assertNoCall(t, 30*time.Millisecond)
		})
	}
}

func TestBulkSearch_NonWantedOrInactiveVariantsNeverQueue(t *testing.T) {
	for _, variant := range []struct {
		name, status, format string
	}{
		{"review", models.VariantReview, models.MediaTypeEbook},
		{"downloading", models.VariantDownloading, models.MediaTypeEbook},
		{"imported", models.VariantImported, models.MediaTypeEbook},
		{"inactive format", models.VariantWanted, models.MediaTypeAudiobook},
	} {
		for _, route := range []string{"book", "wanted", "author"} {
			t.Run(variant.name+"/"+route, func(t *testing.T) {
				searcher := newMockBookSearcher()
				h, _, books, author, ctx := bulkFixtureWithSearcher(t, searcher)
				book := mustCreateBook(t, books, ctx, &models.Book{
					ForeignID: "OL_INACTIVE", AuthorID: author.ID, Title: "No active copy", SortTitle: "no active copy",
					Status: models.BookStatusWanted, MediaType: models.MediaTypeEbook, Monitored: true,
					MetadataProvider: "openlibrary", Genres: []string{},
				})
				copies, err := books.VariantRepo().EnsureWanted(ctx, book.ID, variant.format, []string{"rum"})
				if err != nil {
					t.Fatal(err)
				}
				if variant.status != models.VariantWanted {
					if err := books.VariantRepo().SetStatus(ctx, copies[0].ID, variant.status); err != nil {
						t.Fatal(err)
					}
				}
				id, handler := book.ID, h.BooksBulk
				switch route {
				case "wanted":
					handler = h.WantedBulk
				case "author":
					id, handler = author.ID, h.AuthorsBulk
				}
				rec := postBulk(t, handler, fmt.Sprintf(`{"ids":[%d],"action":"search"}`, id))
				result := decodeBulk(t, rec).Results[strconv.FormatInt(id, 10)]
				if !result.OK || result.Queued || result.SearchSkipped == "" {
					t.Fatalf("inactive %s: %+v; want ok, not queued and a skip reason", variant.name, result)
				}
				searcher.assertNoCall(t, 30*time.Millisecond)
			})
		}
	}
}

func TestBulkSearch_DuplicateSelectedIDQueuesVariantOnce(t *testing.T) {
	searchPaceInterval = 0
	t.Cleanup(func() { searchPaceInterval = 3 * time.Second })
	for _, route := range []string{"book", "wanted", "author"} {
		t.Run(route, func(t *testing.T) {
			searcher := newMockBookSearcher()
			h, _, books, author, ctx := bulkFixtureWithSearcher(t, searcher)
			book := importedBookWithWantedRomanian(t, books, author.ID, ctx)
			id, handler := book.ID, h.BooksBulk
			switch route {
			case "wanted":
				handler = h.WantedBulk
			case "author":
				id, handler = author.ID, h.AuthorsBulk
			}
			rec := postBulk(t, handler, fmt.Sprintf(`{"ids":[%d,%d],"action":"search"}`, id, id))
			if result := decodeBulk(t, rec).Results[strconv.FormatInt(id, 10)]; !result.OK || !result.Queued {
				t.Fatalf("duplicate search result: %+v", result)
			}
			if got := searcher.waitForCall(t, time.Second); got.ID != book.ID {
				t.Fatalf("searched book %d, want %d", got.ID, book.ID)
			}
			searcher.assertNoCall(t, 40*time.Millisecond)
		})
	}
}

func TestBulkSearch_AuthorOwnsOnlyItsOwnVariantBooks(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	searchPaceInterval = 0
	t.Cleanup(func() { searchPaceInterval = 3 * time.Second })
	f := seedTwoUserBulk(t)
	searcher := newMockBookSearcher()
	f.h.searcher = searcher
	ctx := context.Background()
	// A second user's book can be filed under an author Alice owns. An
	// author search must not dispatch the other user's copy.
	other := mustCreateBook(t, f.books, ctx, &models.Book{
		ForeignID: "OL_SHARED_BOB", AuthorID: f.a1.ID, Title: "Bob's Copy", SortTitle: "bobs copy",
		Status: models.BookStatusWanted, MediaType: models.MediaTypeEbook, Monitored: true,
		MetadataProvider: "openlibrary", Genres: []string{},
	})
	setOwner(t, f.database, "books", other.ID, f.u2)
	if _, err := f.books.VariantRepo().EnsureWanted(ctx, other.ID, models.MediaTypeEbook, []string{"rum"}); err != nil {
		t.Fatal(err)
	}
	rec := postBulkAs(t, f.h.AuthorsBulk, fmt.Sprintf(`{"ids":[%d],"action":"search"}`, f.a1.ID), f.u1, "user")
	if result := decodeBulk(t, rec).Results[strconv.FormatInt(f.a1.ID, 10)]; !result.OK || !result.Queued {
		t.Fatalf("owned author search: %+v", result)
	}
	if got := searcher.waitForCall(t, time.Second); got.ID != f.b1.ID {
		t.Fatalf("searched book %d, want only Alice's %d", got.ID, f.b1.ID)
	}
	searcher.assertNoCall(t, 40*time.Millisecond)
}

func TestBulkMonitor_ImportedBookWithWantedVariantQueuesOnNewMonitor(t *testing.T) {
	searcher := newMockBookSearcher()
	h, _, books, author, ctx := bulkFixtureWithSearcher(t, searcher)
	book := importedBookWithWantedRomanian(t, books, author.ID, ctx)
	book.Monitored = false
	if err := books.Update(ctx, book); err != nil {
		t.Fatal(err)
	}
	rec := postBulk(t, h.BooksBulk, fmt.Sprintf(`{"ids":[%d],"action":"monitor"}`, book.ID))
	if result := decodeBulk(t, rec).Results[strconv.FormatInt(book.ID, 10)]; !result.OK || !result.Queued {
		t.Fatalf("newly monitored wanted Romanian copy: %+v", result)
	}
	if got := searcher.waitForCall(t, time.Second); got.ID != book.ID {
		t.Fatalf("searched book %d, want %d", got.ID, book.ID)
	}
	searcher.assertNoCall(t, 40*time.Millisecond)
}

func TestBulkSearch_VariantBookRespectsMonitorExcludeAndSkip(t *testing.T) {
	for _, state := range []string{"unmonitored", "excluded", "skipped"} {
		for _, route := range []string{"book", "wanted", "author"} {
			t.Run(state+"/"+route, func(t *testing.T) {
				searcher := newMockBookSearcher()
				h, _, books, author, ctx := bulkFixtureWithSearcher(t, searcher)
				book := importedBookWithWantedRomanian(t, books, author.ID, ctx)
				switch state {
				case "unmonitored":
					book.Monitored = false
					if err := books.Update(ctx, book); err != nil {
						t.Fatal(err)
					}
				case "excluded":
					if err := books.SetExcluded(ctx, book.ID, true); err != nil {
						t.Fatal(err)
					}
				case "skipped":
					book.Status = models.BookStatusSkipped
					if err := books.Update(ctx, book); err != nil {
						t.Fatal(err)
					}
				}
				id, handler := book.ID, h.BooksBulk
				switch route {
				case "wanted":
					handler = h.WantedBulk
				case "author":
					id, handler = author.ID, h.AuthorsBulk
				}
				rec := postBulk(t, handler, fmt.Sprintf(`{"ids":[%d],"action":"search"}`, id))
				if result := decodeBulk(t, rec).Results[strconv.FormatInt(id, 10)]; !result.OK || result.Queued || result.SearchSkipped == "" {
					t.Fatalf("%s book: %+v; want skipped", state, result)
				}
				searcher.assertNoCall(t, 30*time.Millisecond)
			})
		}
	}
}

func TestBulkSearch_VariantsKeepOwnerAndAutoGrabGuards(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := seedTwoUserBulk(t)
	searcher := newMockBookSearcher()
	f.h.searcher = searcher
	ctx := context.Background()
	copies, err := f.books.VariantRepo().EnsureWanted(ctx, f.b1.ID, models.MediaTypeEbook, []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.books.TrackVariantFile(ctx, f.b1.ID, copies[0].ID, models.MediaTypeEbook, "/library/Alice-eng.epub", "eng", "verified"); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"book", "wanted", "author"} {
		id, handler := f.b1.ID, f.h.BooksBulk
		switch route {
		case "wanted":
			handler = f.h.WantedBulk
		case "author":
			id, handler = f.a1.ID, f.h.AuthorsBulk
		}
		t.Run(route+" wrong owner", func(t *testing.T) {
			rec := postBulkAs(t, handler, fmt.Sprintf(`{"ids":[%d],"action":"search"}`, id), f.u2, "user")
			if got := decodeBulk(t, rec).Results[strconv.FormatInt(id, 10)]; got.OK || got.Queued {
				t.Fatalf("wrong owner: %+v", got)
			}
			searcher.assertNoCall(t, 30*time.Millisecond)
		})
	}
	settings := db.NewSettingsRepo(f.database)
	if err := settings.Set(ctx, "autoGrab.enabled", "false"); err != nil {
		t.Fatal(err)
	}
	f.h.WithSettingsRepo(settings)
	for _, route := range []string{"book", "wanted", "author"} {
		id, handler := f.b1.ID, f.h.BooksBulk
		switch route {
		case "wanted":
			handler = f.h.WantedBulk
		case "author":
			id, handler = f.a1.ID, f.h.AuthorsBulk
		}
		t.Run(route+" switch off", func(t *testing.T) {
			rec := postBulkAs(t, handler, fmt.Sprintf(`{"ids":[%d],"action":"search"}`, id), f.u1, "user")
			if got := decodeBulk(t, rec).Results[strconv.FormatInt(id, 10)]; got.OK || got.Queued || got.Code != autoGrabDisabledCode {
				t.Fatalf("autoGrab off: %+v", got)
			}
			searcher.assertNoCall(t, 30*time.Millisecond)
		})
	}
}
