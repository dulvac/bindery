package db

import (
	"context"
	"slices"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

func TestWantedList_VariantStateAndMediaFilter(t *testing.T) {
	database, author, first := openTestDB(t)
	ctx := context.Background()
	books := NewBookRepo(database)
	variants := books.VariantRepo()
	first.MediaType = models.MediaTypeBoth
	if _, err := database.ExecContext(ctx, "UPDATE books SET media_type='both' WHERE id=?", first.ID); err != nil {
		t.Fatal(err)
	}
	ebook, err := variants.EnsureWanted(ctx, first.ID, "ebook", []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	if err := books.TrackVariantFile(ctx, first.ID, ebook[0].ID, "ebook", "/library/Test-Book-eng.epub", "eng", "verified"); err != nil {
		t.Fatal(err)
	}
	// The audiobook copy is already imported, so an audio-only Wanted filter
	// must not match this book just because the book allows both formats.
	audio, err := variants.EnsureWanted(ctx, first.ID, "audiobook", []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	if err := books.TrackVariantFile(ctx, first.ID, audio[0].ID, "audiobook", "/library/Test-Book-eng.m4b", "eng", "verified"); err != nil {
		t.Fatal(err)
	}
	legacy := &models.Book{ForeignID: "legacy-wanted", AuthorID: author.ID, Title: "Legacy", SortTitle: "Legacy", Status: models.BookStatusWanted, Monitored: true, MediaType: models.MediaTypeAudiobook}
	if err := books.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	review := &models.Book{ForeignID: "review-wanted", AuthorID: author.ID, Title: "Review", SortTitle: "Review", Status: models.BookStatusWanted, Monitored: true, MediaType: models.MediaTypeAudiobook}
	if err := books.Create(ctx, review); err != nil {
		t.Fatal(err)
	}
	pending, err := variants.EnsureWanted(ctx, review.ID, "audiobook", []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	if err := variants.SetStatus(ctx, pending[0].ID, models.VariantReview); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mediaType string
		want            []int64
	}{
		{"all", "", []int64{legacy.ID, first.ID}}, // review row must NOT appear
		{"ebook", "ebook", []int64{first.ID}},
		{"audiobook", "audiobook", []int64{legacy.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, total, err := books.ListPageFiltered(ctx, BookListFilter{Status: models.BookStatusWanted, MediaType: tc.mediaType}, 10, 0)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]int64, 0, len(rows))
			for _, b := range rows {
				ids = append(ids, b.ID)
			}
			if total != len(tc.want) || !slices.Equal(ids, tc.want) {
				t.Errorf("page IDs %v total %d; want %v", ids, total, tc.want)
			}
		})
	}
	rows, err := books.ListByStatusAndUser(ctx, models.BookStatusWanted, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != legacy.ID || rows[1].ID != first.ID {
		t.Errorf("unpaged wanted = %+v, want Legacy and Test Book", rows)
	}
}

// A wanted audiobook row is kept on disk when the user narrows the book to
// ebook, but it must not count as a missing copy until audiobook is requested
// again. Both paged counts and the bare Arr-compatible list share this rule.
func TestWantedList_InactiveFormatSurvivesToggleWithoutPhantomRow(t *testing.T) {
	database, author, first := openTestDB(t)
	ctx := context.Background()
	books := NewBookRepo(database)
	variants := books.VariantRepo()
	first.MediaType = models.MediaTypeBoth
	if err := books.Update(ctx, first); err != nil {
		t.Fatal(err)
	}
	ebook, err := variants.EnsureWanted(ctx, first.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	audio, err := variants.EnsureWanted(ctx, first.ID, models.MediaTypeAudiobook, []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	if err := books.TrackVariantFile(ctx, first.ID, ebook[0].ID, models.MediaTypeEbook, "/library/Test-Book-eng.epub", "eng", "verified"); err != nil {
		t.Fatal(err)
	}
	legacy := &models.Book{ForeignID: "legacy-ebook", AuthorID: author.ID, Title: "Legacy", SortTitle: "Legacy", Status: models.BookStatusWanted, Monitored: true, MediaType: models.MediaTypeEbook}
	if err := books.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}

	check := func(name string, wantAll, wantAudio, wantBoth []int64) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				format string
				want   []int64
			}{
				{"", wantAll},
				{models.MediaTypeEbook, []int64{legacy.ID}},
				{models.MediaTypeAudiobook, wantAudio},
				{models.MediaTypeBoth, wantBoth},
			} {
				t.Run("filter-"+tc.format, func(t *testing.T) {
					rows, total, err := books.ListPageFiltered(ctx, BookListFilter{Status: models.BookStatusWanted, MediaType: tc.format}, 1, 0)
					if err != nil {
						t.Fatal(err)
					}
					if total != len(tc.want) {
						t.Errorf("total = %d, want %d", total, len(tc.want))
					}
					if len(rows) != min(1, len(tc.want)) || (len(rows) > 0 && rows[0].ID != tc.want[0]) {
						t.Errorf("first page = %+v, want first of %v", rows, tc.want)
					}
					if len(tc.want) > 1 {
						second, pageTotal, err := books.ListPageFiltered(ctx, BookListFilter{Status: models.BookStatusWanted, MediaType: tc.format}, 1, 1)
						if err != nil || pageTotal != len(tc.want) || len(second) != 1 || second[0].ID != tc.want[1] {
							t.Errorf("second page = %+v, total %d, err %v; want %d", second, pageTotal, err, tc.want[1])
						}
					}
				})
			}
			bare, err := books.ListByStatusAndUser(ctx, models.BookStatusWanted, 0)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]int64, 0, len(bare))
			for _, b := range bare {
				ids = append(ids, b.ID)
			}
			if !slices.Equal(ids, wantAll) {
				t.Errorf("bare IDs = %v, want %v", ids, wantAll)
			}
		})
	}

	check("both", []int64{legacy.ID, first.ID}, []int64{first.ID}, []int64{first.ID})
	current, err := books.GetByID(ctx, first.ID)
	if err != nil || current == nil || current.EbookFilePath == "" {
		t.Fatalf("tracked English ebook: %+v, %v", current, err)
	}
	current.MediaType = models.MediaTypeEbook
	current.ReevaluateStatus()
	if current.Status != models.BookStatusImported {
		t.Fatalf("ebook-only book with English file should be imported: %+v", current)
	}
	if err := books.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	check("ebook-only", []int64{legacy.ID}, nil, nil)
	kept, err := variants.ListByBook(ctx, first.ID)
	if err != nil || len(kept) != 2 || !slices.ContainsFunc(kept, func(v models.BookVariant) bool { return v.ID == audio[0].ID && v.Status == models.VariantWanted }) {
		t.Fatalf("inactive Romanian audiobook must remain wanted for re-enable: %+v, %v", kept, err)
	}
	current.MediaType = models.MediaTypeBoth
	current.ReevaluateStatus()
	if err := books.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	check("both-again", []int64{legacy.ID, first.ID}, []int64{first.ID}, []int64{first.ID})
}
