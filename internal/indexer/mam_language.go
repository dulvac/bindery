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
		// Restrict short codes to the existing release-language vocabulary;
		// an arbitrary group name in brackets is not language evidence.
		if releaseLanguageCodeSet[code] || code == "rum" {
			codes = append(codes, code)
		}
	}
	codes = slices.Compact(slices.Sorted(slices.Values(codes)))
	return codes
}

// ForRequestedLanguage removes explicit mismatches and puts a known match
// before unknown releases of the same format. It leaves the existing order
// between formats untouched, so format/profile quality stays in control.
func ForRequestedLanguage(results []newznab.SearchResult, language string) (kept []newznab.SearchResult) {
	kept, _ = SplitForRequestedLanguage(results, language)
	return kept
}

// SplitForRequestedLanguage also returns per-release reasons for interactive
// search's filtered section and debug panel. Unknown titles remain eligible.
func SplitForRequestedLanguage(results []newznab.SearchResult, language string) (kept []newznab.SearchResult, dropped []DroppedRelease) {
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

	// Only re-order comparable candidates. A release with multiple format
	// tokens, a retail/unabridged edition or an ISBN/ASIN can earn a larger
	// ranking boost than the language match, as can an indexer priority.
	positions := make(map[string][]int)
	for i, r := range kept {
		parsed := ParseRelease(r.Title)
		quality := strings.Join(ReleaseFormats(r.Title), ",") + "|" +
			strconv.FormatBool(parsed.Retail) + "|" + strconv.FormatBool(parsed.Unabridged) + "|" +
			strconv.FormatBool(parsed.Abridged) + "|" + parsed.ISBN + "|" + parsed.ASIN + "|" +
			strconv.Itoa(r.IndexerPriority)
		positions[quality] = append(positions[quality], i)
	}
	for _, indices := range positions {
		group := make([]newznab.SearchResult, len(indices))
		for j, i := range indices {
			group[j] = kept[i]
		}
		slices.SortStableFunc(group, func(a, b newznab.SearchResult) int {
			matchA := slices.Equal(ReleaseLanguages(a.Title), []string{language})
			matchB := slices.Equal(ReleaseLanguages(b.Title), []string{language})
			switch {
			case matchA && !matchB:
				return -1
			case matchB && !matchA:
				return 1
			default:
				return 0
			}
		})
		for j, i := range indices {
			kept[i] = group[j]
		}
	}
	return kept, dropped
}
