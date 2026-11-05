package indexer

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/vavallee/bindery/internal/indexer/newznab"
	"github.com/vavallee/bindery/internal/models"
)

// A MAM language code is meaningful only in a bracket with a book or audio
// format. In particular, ROM in an ordinary title is not a Romanian tag.
var mamLanguageMarker = regexp.MustCompile(`(?i)\[([a-z]{2,3})\s*/\s*(EPUB|AZW3|MOBI|PDF|M4B|MP3|FLAC)\]`)

// ReleaseLanguages returns language evidence from release-name tags, including
// MAM's format-qualified tags and the existing word, scene and glued tags.
// An empty result means unknown, not English. Multiple codes are ambiguous
// until individual files in the release can be inspected.
func ReleaseLanguages(title string) []string {
	codes := append(releaseLanguageCodes(NormalizeRelease(title)), sceneLanguageCodes(title)...)
	codes = append(codes, gluedLanguageCodes(title)...)
	for _, match := range mamLanguageMarker.FindAllStringSubmatch(title, -1) {
		code := strings.ToLower(match[1])
		if code == "rom" { // MAM's ROM is Romanian, not the ISO code for Romany.
			code = "rum"
		}
		code = models.NormalizeLanguageCode(code)
		// A format-qualified marker can use any recognized language code,
		// even one without a legacy word tag. Arbitrary group names stay unknown.
		if releaseLanguageCodeSet[code] || models.IsKnownLanguageCode(code) {
			codes = append(codes, code)
		}
	}
	codes = slices.Compact(slices.Sorted(slices.Values(codes)))
	return codes
}

// ForRequestedLanguage removes explicit mismatches and gives a known match
// a bounded preference over comparable unknown releases. Different formats,
// editions and stronger scored evidence keep their original ordering.
func ForRequestedLanguage(results []newznab.SearchResult, language string) (kept []newznab.SearchResult) {
	kept, _ = SplitForRequestedLanguage(results, language)
	return kept
}

// SplitForRequestedLanguage also returns per-release reasons for interactive
// search's filtered section and debug panel. Unknown titles remain eligible.
func SplitForRequestedLanguage(results []newznab.SearchResult, language string, criteria ...MatchCriteria) (kept []newznab.SearchResult, dropped []DroppedRelease) {
	if language == "" {
		return results, nil
	}
	language = models.NormalizeLanguageCode(language)
	kept = make([]newznab.SearchResult, 0, len(results))
	for _, r := range results {
		codes := ReleaseLanguages(r.Title)
		switch {
		case len(codes) == 0:
			// An indexer-supplied language attribute is not release-name
			// evidence. Leave the copy unverified, not labelled English.
			r.Language = ""
			kept = append(kept, r)
		case len(codes) == 1 && codes[0] == language:
			kept = append(kept, r)
		case len(codes) > 1:
			dropped = append(dropped, DroppedRelease{Result: r, Stage: StageLanguage,
				Reason: "release languages " + strings.Join(codes, ",") + " are ambiguous for requested variant " + language + " until files are classified"})
		default:
			dropped = append(dropped, DroppedRelease{Result: r, Stage: StageLanguage,
				Reason: "release language " + codes[0] + " does not match requested variant " + language})
		}
	}

	// Only re-order comparable candidates by the same composite score the
	// searcher uses. Preserve ties and the order between different formats,
	// editions, years and identifiers. Callers with a book should provide its
	// search criteria; for standalone filtering, separate release years cannot
	// safely be compared without knowing the book's year.
	c := MatchCriteria{RequestedLanguage: language}
	if len(criteria) > 0 {
		c = criteria[0]
		c.RequestedLanguage = language
	}
	ranks := newProfileRanks(c.Profile)
	positions := make(map[string][]int)
	for i, r := range kept {
		parsed := ParseRelease(r.Title)
		quality := strings.Join(ReleaseFormats(r.Title), ",") + "|" +
			strconv.FormatBool(parsed.Retail) + "|" + strconv.FormatBool(parsed.Unabridged) + "|" +
			strconv.FormatBool(parsed.Abridged) + "|" + parsed.ISBN + "|" + parsed.ASIN + "|" +
			strconv.Itoa(parsed.Year) + "|" + strconv.Itoa(r.IndexerPriority)
		positions[quality] = append(positions[quality], i)
	}
	for _, indices := range positions {
		if len(indices) < 2 {
			continue
		}
		type scored struct {
			result newznab.SearchResult
			score  float64
		}
		group := make([]scored, len(indices))
		for j, i := range indices {
			group[j] = scored{kept[i], scoreResult(kept[i], c, ranks)}
		}
		slices.SortStableFunc(group, func(a, b scored) int {
			switch {
			case a.score > b.score:
				return -1
			case a.score < b.score:
				return 1
			default:
				return 0
			}
		})
		for j, i := range indices {
			kept[i] = group[j].result
		}
	}
	return kept, dropped
}
