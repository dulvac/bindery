package db

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

func TestRequestRepoMergePendingRejectsNewCartesianPairs(t *testing.T) {
	for _, tc := range []struct {
		name, originalFormat, nextFormat string
		originalLanguages, nextLanguages []string
	}{
		{"ebook to different-language audio", "ebook", "audiobook", []string{"eng"}, []string{"rum"}},
		{"both to one different-language format", "both", "ebook", []string{"eng"}, []string{"rum"}},
		{"two-language ebook to single-language audio", "ebook", "audiobook", []string{"eng", "rum"}, []string{"eng"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, repo, users := newRequestsFixture(t)
			ctx := context.Background()
			owner := mustUser(t, users, "reader")
			original := &models.LibraryRequest{
				OwnerUserID: owner.ID, Kind: models.RequestKindBook, ForeignID: "OL1W",
				MediaType: tc.originalFormat, Languages: tc.originalLanguages,
				PayloadJSON: `{"mediaType":"` + tc.originalFormat + `","languages":["eng"]}`,
			}
			if err := repo.Create(ctx, original, 0); err != nil {
				t.Fatal(err)
			}
			before, err := repo.GetByID(ctx, original.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repo.MergePending(ctx, original.ID, owner.ID, tc.nextFormat, tc.nextLanguages, tc.originalFormat); !errors.Is(err, ErrRequestPairsExpand) {
				t.Fatalf("merge = %v, want ErrRequestPairsExpand", err)
			}
			after, err := repo.GetByID(ctx, original.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.MediaType != before.MediaType || after.LanguagesJSON != before.LanguagesJSON || after.PayloadJSON != before.PayloadJSON {
				t.Fatalf("rejected merge mutated row: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestRequestRepoMergePendingKeepsOnlySelectedCartesianPairs(t *testing.T) {
	for _, tc := range []struct {
		name, originalFormat, nextFormat, wantFormat    string
		originalLanguages, nextLanguages, wantLanguages []string
	}{
		{"same format new language", "ebook", "ebook", "ebook", []string{"eng"}, []string{"rum"}, []string{"eng", "rum"}},
		{"other format same language", "ebook", "audiobook", "both", []string{"eng"}, []string{"eng"}, []string{"eng"}},
		{"deliberate both formats for both languages", "ebook", "both", "both", []string{"eng"}, []string{"eng", "rum"}, []string{"eng", "rum"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, repo, users := newRequestsFixture(t)
			ctx := context.Background()
			owner := mustUser(t, users, "reader")
			original := &models.LibraryRequest{
				OwnerUserID: owner.ID, Kind: models.RequestKindBook, ForeignID: "OL1W",
				MediaType: tc.originalFormat, Languages: tc.originalLanguages,
				PayloadJSON: `{"mediaType":"` + tc.originalFormat + `","languages":["eng"]}`,
			}
			if err := repo.Create(ctx, original, 0); err != nil {
				t.Fatal(err)
			}
			merged, err := repo.MergePending(ctx, original.ID, owner.ID, tc.nextFormat, tc.nextLanguages, tc.originalFormat)
			if err != nil {
				t.Fatal(err)
			}
			if merged.MediaType != tc.wantFormat || !reflect.DeepEqual(merged.Languages, tc.wantLanguages) {
				t.Fatalf("merged = %s/%v, want %s/%v", merged.MediaType, merged.Languages, tc.wantFormat, tc.wantLanguages)
			}
		})
	}
}
