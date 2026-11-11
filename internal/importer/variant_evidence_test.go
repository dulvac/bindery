package importer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

func TestAssessVariantEvidence_EbookTextOverrulesDeclaredLanguage(t *testing.T) {
	// Each EPUB was generated from original, synthetic garden prose. The OPF
	// says en in all these cases, including the Romanian and mixed books.
	cases := []struct {
		name, fixture, requested, detected, verification string
		wrongLanguage                                    bool
	}{
		{"Romanian text for English request", "romanian-en-opf.epub", "eng", "rum", "unverified", true},
		{"Romanian text for Romanian request", "romanian-en-opf.epub", "rum", "rum", "verified", false},
		{"English text for English request", "english.epub", "eng", "eng", "verified", false},
		{"French text for French request", "french.epub", "fre", "fre", "verified", false},
		{"mixed chapters do not claim English", "mixed.epub", "eng", "", "unverified", false},
		{"bilingual chapters do not claim English", "intra-mixed.epub", "eng", "", "unverified", false},
		{"short chapters do not claim English", "short.epub", "eng", "", "unverified", false},
		{"front matter does not claim English", "front-only.epub", "eng", "", "unverified", false},
		{"unsupported prose does not claim English", "spanish.epub", "eng", "", "unverified", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			book := &models.Book{ID: 11, Title: "English catalogue label"}
			variant := &models.BookVariant{BookID: book.ID, MediaType: models.MediaTypeEbook, Language: tc.requested}
			download := &models.Download{ID: 42, Title: "Unmarked ebook"}
			got, err := (&Scanner{}).AssessVariantEvidence(context.Background(), download, book,
				&models.Author{Name: "Sample Author"}, variant, []string{filepath.Join("testdata", tc.fixture)}, false)
			if err != nil {
				t.Fatal(err)
			}
			if got.DetectedLanguage != tc.detected || got.Verification != tc.verification ||
				got.WrongLanguage != tc.wrongLanguage || got.WrongWork {
				t.Errorf("got %+v, want detected=%q verification=%q wrongLanguage=%t wrongWork=false", got, tc.detected, tc.verification, tc.wrongLanguage)
			}
		})
	}
}

func TestAssessVariantEvidence_ExplicitLanguageAndExactOverride(t *testing.T) {
	book := &models.Book{ID: 11, Title: "English catalogue label"}
	for _, tc := range []struct {
		name, release, requested, fixture, detected, verification string
		override, wrongLanguage                                   bool
	}{
		{"marked Romanian without override", "English catalogue label [ROM / EPUB]", "eng", "short.epub", "", "unverified", false, true},
		{"marked Romanian with exact override", "English catalogue label [ROM / EPUB]", "eng", "short.epub", "", "unverified", true, false},
		{"confident Romanian text without override", "English catalogue label [ENG / EPUB]", "eng", "romanian-en-opf.epub", "rum", "unverified", false, true},
		{"confident Romanian text with exact override", "English catalogue label [ENG / EPUB]", "eng", "romanian-en-opf.epub", "rum", "unverified", true, false},
		{"matching marker cannot verify short text", "English catalogue label [ENG / EPUB]", "eng", "short.epub", "", "unverified", false, false},
		{"English as book title is not a language marker", "English catalogue label", "rum", "romanian-en-opf.epub", "rum", "verified", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&Scanner{}).AssessVariantEvidence(context.Background(), &models.Download{ID: 42, Title: tc.release}, book,
				&models.Author{Name: "Sample Author"}, &models.BookVariant{BookID: book.ID, MediaType: models.MediaTypeEbook, Language: tc.requested},
				[]string{filepath.Join("testdata", tc.fixture)}, tc.override)
			if err != nil {
				t.Fatal(err)
			}
			if got.DetectedLanguage != tc.detected || got.Verification != tc.verification || got.WrongLanguage != tc.wrongLanguage || got.WrongWork {
				t.Errorf("got %+v, want detected=%q verification=%q wrongLanguage=%t wrongWork=false", got, tc.detected, tc.verification, tc.wrongLanguage)
			}
		})
	}
}

func TestAssessVariantEvidence_EbookWrongWorkBeforeLanguageOverride(t *testing.T) {
	// The altered OPF identifies a wholly different synthetic work by a
	// different author; its original chapter text remains Romanian.
	path := rewriteLanguageEPUB(t, "romanian-en-opf.epub", func(entries map[string][]byte) {
		opf := string(entries["OEBPS/content.opf"])
		opf = strings.Replace(opf, "English catalogue label", "The Other Garden", 1)
		opf = strings.Replace(opf, "</dc:title>", "</dc:title><dc:creator>Other Writer</dc:creator>", 1)
		entries["OEBPS/content.opf"] = []byte(opf)
	})
	book := &models.Book{ID: 11, Title: "English catalogue label"}
	for _, override := range []bool{false, true} {
		got, err := (&Scanner{}).AssessVariantEvidence(context.Background(), &models.Download{ID: 42, Title: "Unmarked ebook"}, book,
			&models.Author{Name: "Sample Author"}, &models.BookVariant{BookID: book.ID, MediaType: models.MediaTypeEbook, Language: "eng"}, []string{path}, override)
		if err != nil {
			t.Fatal(err)
		}
		if !got.WrongWork || got.Verification != "unverified" || got.DetectedLanguage != "rum" || !strings.Contains(got.Reason, "work") {
			t.Errorf("override=%t: wrong work must take priority, got %+v", override, got)
		}
	}
}

func TestAssessVariantEvidence_EbookOverlappingWrongWorkMetadata(t *testing.T) {
	// Whole-word title matching alone accepts a biography containing "Dune".
	// The OPF's independently named author makes this a different work.
	path := rewriteLanguageEPUB(t, "english.epub", func(entries map[string][]byte) {
		opf := string(entries["OEBPS/content.opf"])
		opf = strings.Replace(opf, "English catalogue label", "Dreamer of Dune: A Biography", 1)
		opf = strings.Replace(opf, "</dc:title>", "</dc:title><dc:creator>Brian Herbert</dc:creator>", 1)
		entries["OEBPS/content.opf"] = []byte(opf)
	})
	book := &models.Book{ID: 11, Title: "Dune"}
	got, err := (&Scanner{}).AssessVariantEvidence(context.Background(), &models.Download{ID: 42, Title: "Dune by Frank Herbert [ENG / EPUB]"}, book,
		&models.Author{Name: "Frank Herbert"}, &models.BookVariant{BookID: book.ID, MediaType: models.MediaTypeEbook, Language: "eng"}, []string{path}, true)
	if err != nil || !got.WrongWork || got.Verification != "unverified" {
		t.Errorf("OPF biography under novel, override true: got (%+v, %v); want WrongWork and unverified", got, err)
	}
}

func TestAssessVariantEvidence_TooManyEbooksRemainUnverified(t *testing.T) {
	// Count candidate files before opening any, even when the last EPUB is
	// unreadable. A damaged entry must not make an oversized pack look small.
	files := make([]string, maxContentCheckEpubReads+1)
	for i := range files {
		files[i] = filepath.Join("testdata", "english.epub")
	}
	files[len(files)-1] = filepath.Join(t.TempDir(), "missing.epub")
	book := &models.Book{ID: 11, Title: "English catalogue label"}
	got, err := (&Scanner{}).AssessVariantEvidence(context.Background(), &models.Download{ID: 42, Title: "Unmarked ebook"}, book,
		&models.Author{Name: "Sample Author"}, &models.BookVariant{BookID: book.ID, MediaType: models.MediaTypeEbook, Language: "eng"}, files, false)
	if err != nil || got.Verification != "unverified" || got.DetectedLanguage != "" || !strings.Contains(got.Reason, "too many") {
		t.Errorf("oversized pack: got (%+v, %v); want unverified with read-limit reason", got, err)
	}
}

func TestAssessVariantEvidence_NonEPUBAndUnreadableTextStayUnverified(t *testing.T) {
	book := &models.Book{ID: 11, Title: "English catalogue label"}
	variant := &models.BookVariant{BookID: book.ID, MediaType: models.MediaTypeEbook, Language: "eng"}
	for _, name := range []string{"book.pdf", "unreadable.epub"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(path, []byte("not an EPUB text sample"), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := (&Scanner{}).AssessVariantEvidence(context.Background(), &models.Download{ID: 42, Title: "English catalogue label"}, book,
				&models.Author{Name: "Sample Author"}, variant, []string{path}, false)
			if err != nil || got.DetectedLanguage != "" || got.Verification != "unverified" || got.WrongLanguage || got.WrongWork {
				t.Errorf("got (%+v, %v); want unknown, unverified, no rejection", got, err)
			}
		})
	}
}

func TestAssessVariantEvidence_UnreadableMetadataCannotHideExplicitLanguageMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unreadable.epub")
	if err := os.WriteFile(path, []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	book := &models.Book{ID: 11, Title: "The Garden"}
	got, err := (&Scanner{}).AssessVariantEvidence(context.Background(), &models.Download{ID: 42, Title: "The Garden [ROM / EPUB]"}, book,
		&models.Author{Name: "Ada Vale"}, &models.BookVariant{BookID: book.ID, MediaType: models.MediaTypeEbook, Language: "eng"}, []string{path}, false)
	if err != nil || !got.WrongLanguage || !strings.Contains(got.Reason, "release language") || got.Verification != "unverified" {
		t.Errorf("unreadable EPUB masked explicit wrong-language marker: got (%+v, %v)", got, err)
	}
}

// writeVariantM4B makes real, tiny MP4 atoms, not a mock of the tag reader.
func writeVariantM4B(t *testing.T, album, artist string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample.m4b")
	data := mp4File()
	if album != "" || artist != "" {
		data = mp4File(
			atom("\xa9nam", dataAtom(1, []byte("Chapter 1"))),
			atom("\xa9alb", dataAtom(1, []byte(album))),
			atom("\xa9ART", dataAtom(1, []byte(artist))),
		)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAssessVariantEvidence_AudioIdentityAndUnknownLanguage(t *testing.T) {
	for _, tc := range []struct {
		name, wanted, wantedAuthor, release, album, tagAuthor string
		override, wrongWork, wrongLanguage                    bool
	}{
		{"biography album and author", "Dune", "Frank Herbert", "Dreamer of Dune: The Biography of Frank Herbert by Brian Herbert [ENG / M4B]", "Dreamer of Dune: The Biography of Frank Herbert", "Brian Herbert", false, true, false},
		{"biography still wrong under override", "Dune", "Frank Herbert", "Dreamer of Dune: The Biography of Frank Herbert by Brian Herbert [ENG / M4B]", "Dreamer of Dune: The Biography of Frank Herbert", "Brian Herbert", true, true, false},
		{"coherent album retitle cannot hide biography release", "Dune", "Frank Herbert", "Dreamer of Dune: The Biography of Frank Herbert by Brian Herbert [ENG / M4B]", "Dune", "Brian Herbert", true, true, false},
		{"authorless release names another work corroborated by tag author", "Dune", "Frank Herbert", "Dreamer of Dune [ENG / M4B]", "Dune", "Brian Herbert", true, true, false},
		{"named release author and matching album outweigh bare narrator artist", "Dune", "Frank Herbert", "Dune Unabridged by Frank Herbert [ENG / M4B]", "Dune", "Simon Vance", false, false, false},
		{"retitled release with matching author is not wrong work", "Dune", "Frank Herbert", "Duna [ENG / M4B]", "Dune", "Frank Herbert", false, false, false},
		{"same author sequel release and album are another work", "Dune", "Frank Herbert", "Dune Messiah by Frank Herbert [ENG / M4B]", "Dune Messiah", "Frank Herbert", false, true, false},
		{"explicit same author sequel release is another work even without tags", "Dune", "Frank Herbert", "Dune Messiah by Frank Herbert [ENG / M4B]", "", "", false, true, false},
		{"narrator after same author cannot hide sequel without tags", "Dune", "Frank Herbert", "Dune Messiah by Frank Herbert - Narrated by Simon Vance [ENG / M4B]", "", "", false, true, false},
		{"dotted author credit cannot hide sequel without tags", "Dune", "Frank Herbert", "Dune.Messiah.by.Frank.Herbert.[ENG / M4B]", "", "", false, true, false},
		{"dotted author first cannot hide sequel without tags", "Dune", "Frank Herbert", "Frank.Herbert.-.Dune.Messiah.[ENG / M4B]", "", "", false, true, false},
		{"dotted byline edition cannot hide sequel without tags", "Dune", "Frank Herbert", "Dune.Messiah.[Unabridged].by.Frank.Herbert.[ENG / M4B]", "", "", false, true, false},
		{"narrator after same author leaves exact novel eligible", "Dune", "Frank Herbert", "Dune by Frank Herbert - Narrated by Simon Vance [ENG / M4B]", "Dune", "Simon Vance", false, false, false},
		{"requested author may also narrate the novel", "Dune", "Frank Herbert", "Dune Narrated by Frank Herbert [ENG / M4B]", "Dune", "Frank Herbert", false, false, false},
		{"bracketed author narrator is not a different work", "Dune", "Frank Herbert", "Dune [Narrated by Frank Herbert] [ENG / M4B]", "Dune", "Frank Herbert", false, false, false},
		{"bracketed read by author is not a different work", "Dune", "Frank Herbert", "Dune [Read by Frank Herbert] [ENG / M4B]", "Dune", "Frank Herbert", false, false, false},
		{"bracketed performed by author is not a different work", "Dune", "Frank Herbert", "Dune [Performed by Frank Herbert] [ENG / M4B]", "Dune", "Frank Herbert", false, false, false},
		{"bracketed presented by author is not a different work", "Dune", "Frank Herbert", "Dune [Presented by Frank Herbert] [ENG / M4B]", "Dune", "Frank Herbert", false, false, false},
		{"bracketed told by author is not a different work", "Dune", "Frank Herbert", "Dune [Told by Frank Herbert] [ENG / M4B]", "Dune", "Frank Herbert", false, false, false},
		{"performed by label with distinct author credit stays wrong", "Dune", "Frank Herbert", "Dune Messiah [Performed by Frank Herbert] by Frank Herbert [ENG / M4B]", "", "", false, true, false},
		{"exact release cannot hide consistent same author sequel album", "Dune", "Frank Herbert", "Dune by Frank Herbert [ENG / M4B]", "Dune Messiah", "Frank Herbert", false, true, false},
		{"same author colon sequel album", "Dune", "Frank Herbert", "Dune: Messiah by Frank Herbert [ENG / M4B]", "Dune: Messiah", "Frank Herbert", false, true, false},
		{"exact release cannot hide colon sequel album", "Dune", "Frank Herbert", "Dune by Frank Herbert [ENG / M4B]", "Dune: Messiah", "Frank Herbert", false, true, false},
		{"same author sequel with novel suffix", "Dune", "Frank Herbert", "Dune Messiah: A Novel by Frank Herbert [ENG / M4B]", "Dune Messiah: A Novel", "Frank Herbert", false, true, false},
		{"exact release cannot hide sequel bracketed unabridged album", "Dune", "Frank Herbert", "Dune by Frank Herbert [ENG / M4B]", "Dune Messiah [Unabridged]", "Frank Herbert", false, true, false},
		{"exact release cannot hide sequel parenthesized novel album", "Dune", "Frank Herbert", "Dune by Frank Herbert [ENG / M4B]", "Dune Messiah (A Novel)", "Frank Herbert", false, true, false},
		{"exact release cannot hide sequel narrator label", "Dune", "Frank Herbert", "Dune by Frank Herbert [ENG / M4B]", "Dune Messiah [Narrated by Simon Vance]", "Frank Herbert", false, true, false},
		{"exact release cannot hide sequel unabridged format label", "Dune", "Frank Herbert", "Dune by Frank Herbert [ENG / M4B]", "Dune Messiah [Unabridged MP3]", "Frank Herbert", false, true, false},
		{"illustrated novel album is not another work", "Dune", "Frank Herbert", "Dune by Frank Herbert [ENG / M4B]", "Dune (Illustrated)", "Frank Herbert", false, false, false},
		{"dramatized novel album is not another work", "Dune", "Frank Herbert", "Dune by Frank Herbert [ENG / M4B]", "Dune (Dramatized)", "Frank Herbert", false, false, false},
		{"same author edition qualifier stays eligible", "Dune", "Frank Herbert", "Dune (Unabridged) by Frank Herbert [ENG / M4B]", "Dune (Unabridged)", "Frank Herbert", false, false, false},
		{"clean release cannot hide wrong tagged work", "Dune", "Frank Herbert", "Dune by Frank Herbert [ENG / M4B]", "Dreamer of Dune: The Biography of Frank Herbert", "Brian Herbert", false, true, false},
		{"matching title but release and tags agree on wrong author", "Dune", "Frank Herbert", "Dune by Brian Herbert [ENG / M4B]", "Dune", "Brian Herbert", true, true, false},
		{"legitimate translation with matching author", "The Garden of Stars", "Ada Vale", "The Garden of Stars [ENG / M4B]", "Le Jardin des Étoiles", "Ada Vale", false, false, false},
		{"matching title without language tags", "The Garden of Stars", "Ada Vale", "The Garden of Stars [ENG / M4B]", "The Garden of Stars", "Ada Vale", false, false, false},
		{"marked Romanian audio for English variant", "The Garden of Stars", "Ada Vale", "The Garden of Stars by Ada Vale [ROM / M4B]", "The Garden of Stars", "Ada Vale", false, false, true},
		{"exact override clears audio marker only", "The Garden of Stars", "Ada Vale", "The Garden of Stars by Ada Vale [ROM / M4B]", "The Garden of Stars", "Ada Vale", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeVariantM4B(t, tc.album, tc.tagAuthor)
			book := &models.Book{ID: 11, Title: tc.wanted}
			got, err := (&Scanner{}).AssessVariantEvidence(context.Background(), &models.Download{ID: 42, Title: tc.release}, book,
				&models.Author{Name: tc.wantedAuthor}, &models.BookVariant{BookID: book.ID, MediaType: models.MediaTypeAudiobook, Language: "eng"}, []string{path}, tc.override)
			if err != nil {
				t.Fatal(err)
			}
			if got.WrongWork != tc.wrongWork || got.WrongLanguage != tc.wrongLanguage || got.DetectedLanguage != "" || got.Verification != "unverified" {
				t.Errorf("got %+v, want wrongWork=%t wrongLanguage=%t, detected=empty and unverified", got, tc.wrongWork, tc.wrongLanguage)
			}
			if tc.wrongWork && !strings.Contains(got.Reason, "work") {
				t.Errorf("wrong work needs an explanation, got %q", got.Reason)
			}
		})
	}
}

func TestAssessVariantEvidence_MP3AlbumsNameSameAuthorSequel(t *testing.T) {
	dir := t.TempDir()
	var files []string
	for i := 1; i <= 18; i++ {
		path := filepath.Join(dir, fmt.Sprintf("%02d.mp3", i))
		if err := os.WriteFile(path, buildID3v23Album(fmt.Sprintf("Chapter %d", i), "Dune Messiah"), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, path)
	}
	book := &models.Book{ID: 11, Title: "Dune"}
	got, err := (&Scanner{}).AssessVariantEvidence(context.Background(),
		&models.Download{ID: 42, Title: "Dune by Frank Herbert [ENG / MP3]"}, book,
		&models.Author{Name: "Frank Herbert"},
		&models.BookVariant{BookID: book.ID, MediaType: models.MediaTypeAudiobook, Language: "eng"}, files, false)
	if err != nil {
		t.Fatal(err)
	}
	if !got.WrongWork || got.Verification != "unverified" || got.DetectedLanguage != "" {
		t.Fatalf("18 MP3 albums explicitly name the sequel: %+v", got)
	}
}

// An explicit author credit is release evidence even when its title exactly
// matches the requested work. Tags cannot make that conflicting credit safe.
func TestAssessVariantEvidence_ExactAudioTitleExplicitAuthor(t *testing.T) {
	for _, tc := range []struct {
		name, release, album, artist    string
		unreadable, override, wrongWork bool
	}{
		{"wrong author with no tags", "Dune by Brian Herbert [ENG / M4B]", "", "", false, false, true},
		{"wrong author with unreadable tags", "Dune by Brian Herbert [ENG / M4B]", "", "", true, false, true},
		{"wrong author role before narrator", "Dune - Brian Herbert - author, Simon Vance - narrator [ENG/M4B]", "", "", false, false, true},
		{"by author role before narrator", "Dune by Brian Herbert - author, Simon Vance - narrator [ENG/M4B]", "", "", false, false, true},
		{"wrong author role after narrator", "Dune - Simon Vance - narrator, Brian Herbert - author [ENG/M4B]", "", "", false, false, true},
		{"matching narrator cannot redeem wrong author role", "Dune - Brian Herbert - author, Frank Herbert - narrator [ENG/M4B]", "Dune", "Frank Herbert", false, false, true},
		{"matching album with bare narrator artist cannot redeem release", "Dune by Brian Herbert [ENG / M4B]", "Dune", "Brian Herbert", false, false, true},
		{"matching album with read-by narrator cannot redeem release", "Dune by Brian Herbert [ENG / M4B]", "Dune", "Read by Brian Herbert", false, false, true},
		{"language override cannot waive wrong author", "Dune by Brian Herbert [ROM / M4B]", "Dune", "Frank Herbert", false, true, true},
		{"correct author with no tags", "Dune by Frank Herbert [ENG / M4B]", "", "", false, false, false},
		{"correct author role before narrator", "Dune - Frank Herbert - author, Brian Herbert - narrator [ENG/M4B]", "", "", false, false, false},
		{"correct author role alone", "Dune - Frank Herbert - author [ENG/M4B]", "", "", false, false, false},
		{"author-first release stays eligible", "Frank Herbert - Dune [ENG / M4B]", "Dune", "Frank Herbert", false, false, false},
		{"correct author with narrator artist", "Dune by Frank Herbert [ENG / M4B]", "Dune", "Brian Herbert", false, false, false},
		{"read by is not an author credit", "Dune - Read by Brian Herbert [ENG / M4B]", "Dune", "", false, false, false},
		{"narrated by is not an author credit", "Dune Narrated by Brian Herbert [ENG / M4B]", "Dune", "Brian Herbert", false, false, false},
		{"by narrator role is not an author", "Dune by Brian Herbert - narrator [ENG/M4B]", "", "", false, false, false},
		{"by narrator role with tags is not an author", "Dune by Brian Herbert - narrator [ENG/M4B]", "Dune", "Brian Herbert", false, false, false},
		{"dash narrator role is not an author", "Dune - Brian Herbert - narrator [ENG/M4B]", "", "", false, false, false},
		{"same author may retitle a translation", "Duna by Frank Herbert [ENG / M4B]", "Dune", "Frank Herbert", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeVariantM4B(t, tc.album, tc.artist)
			if tc.unreadable {
				if err := os.WriteFile(path, []byte("not an MP4"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			book := &models.Book{ID: 11, Title: "Dune"}
			got, err := (&Scanner{}).AssessVariantEvidence(context.Background(),
				&models.Download{ID: 42, Title: tc.release}, book, &models.Author{Name: "Frank Herbert"},
				&models.BookVariant{BookID: book.ID, MediaType: models.MediaTypeAudiobook, Language: "eng"}, []string{path}, tc.override)
			if err != nil {
				t.Fatal(err)
			}
			if got.WrongWork != tc.wrongWork || got.Verification != "unverified" {
				t.Fatalf("release %q: evidence %+v, want wrongWork=%t and unverified", tc.release, got, tc.wrongWork)
			}
			if tc.wrongWork && !strings.Contains(got.Reason, "work") {
				t.Fatalf("wrong work needs an explanation: %+v", got)
			}
		})
	}
}

func TestAssessVariantEvidence_MixedSameAuthorAudioIsNotConfidentWrongWork(t *testing.T) {
	// The legacy content check flags both mixed albums. Only conflicting
	// authors on the other titles corroborate a different work.
	for _, tc := range []struct {
		name, otherAuthor string
		wrongWork         bool
	}{
		{"same author can translate or retitle both tracks", "Frank Herbert", false},
		{"other tracks name a different author", "Brian Herbert", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := []string{
				writeVariantM4B(t, "Dune", "Frank Herbert"),
				writeVariantM4B(t, "Duna", tc.otherAuthor),
				writeVariantM4B(t, "Les Sables", tc.otherAuthor),
			}
			book := &models.Book{ID: 11, Title: "Dune"}
			got, err := (&Scanner{}).AssessVariantEvidence(context.Background(),
				&models.Download{ID: 42, Title: "Dune by Frank Herbert [ENG / M4B]"}, book,
				&models.Author{Name: "Frank Herbert"},
				&models.BookVariant{BookID: book.ID, MediaType: models.MediaTypeAudiobook, Language: "eng"}, files, false)
			if err != nil {
				t.Fatal(err)
			}
			if got.WrongWork != tc.wrongWork || got.WrongLanguage || got.DetectedLanguage != "" || got.Verification != "unverified" {
				t.Errorf("got %+v, want wrongWork=%t and unknown language", got, tc.wrongWork)
			}
		})
	}
}

func TestAssessVariantEvidence_BracketedLanguageSurvivesCatalogueTitle(t *testing.T) {
	book := &models.Book{ID: 11, Title: "English Garden"}
	variant := &models.BookVariant{BookID: book.ID, MediaType: models.MediaTypeAudiobook, Language: "rum"}
	for _, tc := range []struct {
		name, release string
		wrongLanguage bool
	}{
		{"bracketed English is an edition label", "English Garden [English] [M4B]", true},
		{"title word without language label is not evidence", "English Garden [M4B]", false},
		{"unknown bracket remains unverified", "English Garden [ZZZ] [M4B]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeVariantM4B(t, "English Garden", "Ada Vale")
			got, err := (&Scanner{}).AssessVariantEvidence(context.Background(),
				&models.Download{ID: 42, Title: tc.release}, book, &models.Author{Name: "Ada Vale"},
				variant, []string{path}, false)
			if err != nil {
				t.Fatal(err)
			}
			if got.WrongLanguage != tc.wrongLanguage || got.WrongWork || got.DetectedLanguage != "" || got.Verification != "unverified" {
				t.Errorf("release %q: got %+v, want wrongLanguage=%t and unknown language", tc.release, got, tc.wrongLanguage)
			}
			if tc.wrongLanguage && !strings.Contains(got.Reason, "release language") {
				t.Errorf("explicit language mismatch needs release evidence in reason, got %q", got.Reason)
			}
		})
	}
}

func TestAssessVariantEvidence_ExistingAudioRetitleCheckIsUnchanged(t *testing.T) {
	// This check's warn/retitle result remains intact. The variant gate above
	// makes the stricter work decision using conflicting author and release.
	path := writeVariantM4B(t, "Dreamer of Dune: The Biography of Frank Herbert", "Brian Herbert")
	got := (&Scanner{}).checkAudiobookContent(context.Background(), &models.Book{Title: "Dune"},
		&models.Author{Name: "Frank Herbert"}, []string{path})
	if got.flagged || got.mode != contentCheckWarn {
		t.Errorf("existing content check changed: %+v", got)
	}
}
