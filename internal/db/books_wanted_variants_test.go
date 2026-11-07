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
