package indexer

import (
	"slices"
	"testing"

	"github.com/vavallee/bindery/internal/indexer/newznab"
	"github.com/vavallee/bindery/internal/models"
)

// A loose "rom" word match would mistake an ordinary book title for Romanian.
func TestReleaseLanguagesMAMMarkersAndExistingTags(t *testing.T) {
	for _, tc := range []struct {
		title string
		want  []string
	}{
		{"Dune [ROM / EPUB]", []string{"rum"}},
		{"Dune [RON / EPUB]", []string{"rum"}},
		{"Dune [RUM / EPUB]", []string{"rum"}},
		{"Dune [ENG / M4B]", []string{"eng"}},
		{"Dune [FRA / EPUB]", []string{"fre"}},
		{"Dune [FRE / EPUB]", []string{"fre"}},
		{"Dune [rom / mp3]", []string{"rum"}},
		{"Dune [DE / EPUB]", []string{"ger"}},
		{"Dune [ES / EPUB]", []string{"spa"}},
		{"Dune [ZZZ / EPUB]", nil},
		{"Dune [ROM / EPUB] [ENG / EPUB]", []string{"eng", "rum"}},
		{"ROM: A Novel", nil},
		{"Dune [ROM]", nil},
		{"Dune.epub", nil},
		{"Dune FRENCH.epub", []string{"fre"}},
		{"Frank_Herbert-Dune-AUDIOBOOK-WEB-MP3-DE-2023-GRP", []string{"ger"}},
	} {
		t.Run(tc.title, func(t *testing.T) {
			if got := ReleaseLanguages(tc.title); !slices.Equal(got, tc.want) {
				t.Errorf("ReleaseLanguages(%q) = %v; want %v", tc.title, got, tc.want)
			}
		})
	}
}

// Format-qualified codes can be known languages even when no legacy word tag
// spells them. An unknown bracket token is still unverified, not a mismatch.
func TestMAMLanguageRecognizesKnownCodesBeyondLegacyReleaseTags(t *testing.T) {
	for _, tc := range []struct {
		title string
		want  []string
	}{
		{"Dune [HE / EPUB]", []string{"heb"}},
		{"Dune [HEB / EPUB]", []string{"heb"}},
		{"Dune [JA / EPUB]", []string{"jpn"}},
		{"Dune [ZZZ / EPUB]", nil},
		{"Dune [WEB / EPUB]", nil},
	} {
		t.Run(tc.title, func(t *testing.T) {
			if got := ReleaseLanguages(tc.title); !slices.Equal(got, tc.want) {
				t.Errorf("ReleaseLanguages(%q) = %v; want %v", tc.title, got, tc.want)
			}
		})
	}
	in := []newznab.SearchResult{
		{Title: "Dune [HE / EPUB]"},
		{Title: "Dune [ZZZ / EPUB]", Language: "en"},
		{Title: "Dune [ENG / EPUB]"},
	}
	got := ForRequestedLanguage(in, "eng")
	if len(got) != 2 || got[0].Title != in[2].Title || got[1].Title != in[1].Title || got[1].Language != "" {
		t.Errorf("English variant = %+v; want English and unverified, not explicit Hebrew", got)
	}
}

// An explicit mismatch must never fill a variant, but an untagged release
// remains eligible without being described as English.
func TestMAMLanguageForEnglishVariant(t *testing.T) {
	in := []newznab.SearchResult{{Title: "Dune [ROM / EPUB]"}, {Title: "Dune.epub", Language: "en"}, {Title: "Dune [ENG / EPUB]"}, {Title: "Dune [ROM / EPUB] [ENG / EPUB]"}}
	got := ForRequestedLanguage(in, "eng")
	if len(got) != 2 || got[0].Title != in[2].Title || got[1].Title != in[1].Title {
		t.Fatalf("English releases = %#v; want known English before unknown, no Romanian or mixed bundle", got)
	}
	if got[1].Language != "" || in[1].Language != "en" {
		t.Errorf("unknown release must be unverified without mutating indexer evidence: result=%+v original=%+v", got[1], in[1])
	}
	if in[0].Title != "Dune [ROM / EPUB]" || in[1].Title != "Dune.epub" {
		t.Error("variant filtering mutated the original results")
	}
}

func TestMAMLanguageRejectsOtherExplicitMAMCodes(t *testing.T) {
	in := []newznab.SearchResult{{Title: "Dune [DE / EPUB]"}, {Title: "Dune [ENG / EPUB]"}}
	got := ForRequestedLanguage(in, "eng")
	if len(got) != 1 || got[0].Title != in[1].Title {
		t.Errorf("explicit German marker was accepted for English: %+v", got)
	}
}

func TestMAMLanguageForRomanianAndFrenchVariants(t *testing.T) {
	in := []newznab.SearchResult{{Title: "Dune [ENG / M4B]"}, {Title: "Dune [ROM / M4B]"}, {Title: "Dune [FRE / M4B]"}, {Title: "Dune.m4b"}}
	for _, tc := range []struct {
		lang, expected string
	}{{"ro", in[1].Title}, {"fre", in[2].Title}} {
		got := ForRequestedLanguage(in, tc.lang)
		if len(got) != 2 || got[0].Title != tc.expected || got[1].Title != in[3].Title {
			t.Errorf("%q releases = %#v; want matching audio then unknown", tc.lang, got)
		}
	}
}

// Explicit matches outrank unmarked releases of the same format, but the
// variant preference must not overturn the existing format-quality ordering.
func TestMAMLanguageRanksWithinComparableFormat(t *testing.T) {
	in := []newznab.SearchResult{
		{Title: "Dune.pdf"},
		{Title: "Dune.epub", Grabs: 1},
		{Title: "Dune [ENG / EPUB]"},
	}
	got := ForRequestedLanguage(in, "eng")
	if len(got) != 3 || got[0].Title != in[0].Title || got[1].Title != in[2].Title || got[2].Title != in[1].Title {
		t.Fatalf("format ranking and language priority = %#v", got)
	}
	// A multi-format release may outrank plain EPUB under a quality profile.
	// A known EPUB alone must not displace it merely because ParseRelease
	// reduces both to an EPUB format.
	profileRanked := []newznab.SearchResult{{Title: "Dune.azw3.epub"}, {Title: "Dune [ENG / EPUB]"}}
	profileGot := ForRequestedLanguage(profileRanked, "eng")
	if len(profileGot) != 2 || profileGot[0].Title != profileRanked[0].Title {
		t.Errorf("different quality-profile format sets were reordered: %+v", profileGot)
	}
	// The searcher's score also respects format before the language bonus.
	ranked := append([]newznab.SearchResult(nil), in...)
	rankResults(ranked, MatchCriteria{Title: "Dune", MediaType: models.MediaTypeEbook, RequestedLanguage: "eng"})
	if ranked[0].Title != in[2].Title || ranked[1].Title != in[1].Title || ranked[2].Title != in[0].Title {
		t.Fatalf("ranked releases = %#v; want marked EPUB, unknown EPUB, PDF", ranked)
	}
}

// A known-language marker is a modest score preference, not permission to
// undo the ranker's edition-year or popularity ordering in the final filter.
func TestMAMLanguagePreservesRankerYearAndScoreDifferences(t *testing.T) {
	criteria := MatchCriteria{Title: "Dune", MediaType: models.MediaTypeEbook, Year: 2024, RequestedLanguage: "eng"}
	for _, tc := range []struct {
		name, want string
		in         []newznab.SearchResult
	}{
		{"year after sorting", "Dune 2024.epub", []newznab.SearchResult{{Title: "Dune 2024.epub"}, {Title: "Dune 2000 [ENG / EPUB]"}}},
		{"year even if marked was first", "Dune 2024.epub", []newznab.SearchResult{{Title: "Dune 2000 [ENG / EPUB]"}, {Title: "Dune 2024.epub"}}},
		{"popularity after sorting", "Dune.epub", []newznab.SearchResult{{Title: "Dune.epub", Grabs: 100}, {Title: "Dune [ENG / EPUB]"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ranked := append([]newznab.SearchResult(nil), tc.in...)
			rankResults(ranked, criteria)
			got := ForRequestedLanguage(ranked, "eng")
			if len(got) != 2 || got[0].Title != tc.want {
				t.Errorf("ranked and filtered = %+v; want stronger unmarked candidate first", got)
			}
			if scoped, _ := SplitForRequestedLanguage(ranked, "eng", criteria); len(scoped) != 2 || scoped[0].Title != tc.want {
				t.Errorf("criteria-aware split = %+v; want stronger unmarked candidate first", scoped)
			}
		})
	}
}

// A language bonus cannot trump a substantially better edition or an exact
// identifier: those scores matter more than the small known-language bonus.
func TestMAMLanguagePreservesStrongQualityAndIdentifierEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, betterUnknown string
	}{
		{"retail", "Dune RETAIL.epub"},
		{"ISBN", "Dune ISBN 9780441172719.epub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []newznab.SearchResult{{Title: tc.betterUnknown}, {Title: "Dune [ENG / EPUB]"}}
			got := ForRequestedLanguage(in, "eng")
			if len(got) != 2 || got[0].Title != in[0].Title {
				t.Errorf("release-quality evidence displaced by language tag: %+v", got)
			}
		})
	}
}

func TestMAMLanguageKeepsLegacyProfileFilter(t *testing.T) {
	in := []newznab.SearchResult{{Title: "Dune [ROM / EPUB]"}, {Title: "Dune FRENCH.epub"}, {Title: "Dune.epub"}}
	got := FilterByAllowedLanguages(in, []string{"eng", "fre"})
	if len(got) != 2 || got[0].Title != in[1].Title || got[1].Title != in[2].Title {
		t.Errorf("profile filter = %#v; want French and unknown only", got)
	}
}
