package importer

import (
	"path/filepath"
	"testing"
)

func TestVariantDestinationLanguagesAndFormats(t *testing.T) {
	for _, tc := range []struct {
		name, base, language, want string
		folder                     bool
	}{
		{"English EPUB", "/library/books/Author/Title.epub", "eng", "/library/books/Author/Title [eng].epub", false},
		{"Romanian EPUB", "/library/books/Author/Title.epub", "rum", "/library/books/Author/Title [rum].epub", false},
		{"French PDF", "/library/books/Author/Title.pdf", "fre", "/library/books/Author/Title [fre].pdf", false},
		{"Kobo compound extension", "/library/books/Author/Title.kepub.epub", "rum", "/library/books/Author/Title [rum].kepub.epub", false},
		{"case-preserved extension", "/library/books/Author/Title.EPUB", "eng", "/library/books/Author/Title [eng].EPUB", false},
		{"audio folder", "/library/audiobooks/Author/Title", "rum", "/library/audiobooks/Author/Title [rum]", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := VariantDestination(tc.base, tc.language, tc.folder)
			if err != nil || got != tc.want {
				t.Fatalf("VariantDestination(%q, %q, %t) = %q, %v; want %q", tc.base, tc.language, tc.folder, got, err, tc.want)
			}
			if filepath.Dir(got) != filepath.Dir(tc.base) {
				t.Fatalf("destination escaped parent: %q", got)
			}
		})
	}
}

func TestVariantDestinationRejectsInvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		base, language string
		folder         bool
	}{
		{"/library/books/Title.epub", "", false},
		{"/library/books/Title.epub", "deu", false},
		{"/library/books/Title.epub", "../../outside", false},
		{"../library/books/Title.epub", "eng", false},
		{"/library/books/../Title.epub", "eng", false},
		{"/library/books/Title", "eng", false},
		{"/", "eng", true},
		{"/library/audiobooks/..", "eng", true},
	} {
		if got, err := VariantDestination(tc.base, tc.language, tc.folder); err == nil {
			t.Errorf("VariantDestination(%q, %q, %t) = %q, want error", tc.base, tc.language, tc.folder, got)
		}
	}
}

func TestVariantDestinationDoesNotChangeLegacyPath(t *testing.T) {
	legacy := "/library/books/Author/Title.epub"
	first, err := VariantDestination(legacy, "eng", false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := VariantDestination(legacy, "rum", false)
	if err != nil {
		t.Fatal(err)
	}
	if legacy != "/library/books/Author/Title.epub" || first == second || first == legacy || second == legacy {
		t.Fatalf("legacy=%q, eng=%q, rum=%q; each new destination must be distinct", legacy, first, second)
	}
}
