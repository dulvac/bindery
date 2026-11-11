package indexer

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/vavallee/bindery/internal/indexer/newznab"
)

var (
	languageFormatTail  = regexp.MustCompile(`(?i)(?:\s|\.)*\[\s*[a-z]{2,3}\s*/\s*[a-z0-9 ]+\]\s*$`)
	terminalParenLabel  = regexp.MustCompile(`(?:\s|\.)*\(([^()]*)\)\s*$`)
	terminalSquareLabel = regexp.MustCompile(`(?:\s|\.)*\[([^\[\]]*)\]\s*$`)
	byCreditMarker      = regexp.MustCompile(`(?i)[\s._]+by[\s._]+`)
)

// Keep the role set in step with importer.narratorCreditRe. A narrator's
// by-credit, even when the author did the reading, is not a book-author credit.
func knownNarratorRole(word string) bool {
	switch word {
	case "read", "narrated", "performed", "presented", "told":
		return true
	}
	return false
}

func knownTerminalEditionLabel(label string) bool {
	words := strings.Fields(NormalizeRelease(label))
	if len(words) == 0 {
		return false
	}
	if isEditionSuffix(words) {
		return true
	}
	if len(words) >= 3 && knownNarratorRole(words[0]) && words[1] == "by" {
		return true
	}
	if len(words) == 2 && (words[0] == "unabridged" || words[0] == "abridged") {
		switch words[1] {
		case "mp3", "m4b", "flac", "aac", "ogg", "opus":
			return true
		}
	}
	if len(words) != 1 {
		return false
	}
	switch words[0] {
	case "dramatized", "retail", "repack", "epub", "mobi", "azw3", "m4b", "mp3", "pdf", "flac",
		"eng", "en", "rom", "rum", "fre", "fr":
		return true
	}
	return false
}

func trimTerminalEditionLabels(title string) string {
	for {
		trimmed := false
		for _, pattern := range []*regexp.Regexp{terminalParenLabel, terminalSquareLabel} {
			match := pattern.FindStringSubmatchIndex(title)
			if match != nil && knownTerminalEditionLabel(title[match[2]:match[3]]) {
				title = strings.TrimSpace(title[:match[0]])
				trimmed = true
				break
			}
		}
		if !trimmed {
			return title
		}
	}
}

// ObviousLongerWorkTitle reports a named, longer work after a short requested
// title. It does not infer a different work from a translated title or an
// incomplete set of tags. A known edition or narrator suffix is not a work.
func ObviousLongerWorkTitle(wanted, candidate string) bool {
	wanted = strings.TrimSpace(wanted)
	if len(newznab.SigWords(wanted)) != 1 {
		return false
	}
	candidate = trimTerminalEditionLabels(strings.TrimSpace(languageFormatTail.ReplaceAllString(candidate, "")))
	if i := strings.LastIndex(candidate, ":"); i >= 0 &&
		isEditionSuffix(strings.Fields(strings.ToLower(candidate[i+1:]))) {
		candidate = strings.TrimSpace(candidate[:i])
	}
	// Only one unpunctuated extra word is confident evidence here. Scene
	// releases also use a dot between words. Subtitles, positions and longer
	// edition labels stay with the existing contextual relevance rules.
	if len(candidate) <= len(wanted) || !strings.EqualFold(candidate[:len(wanted)], wanted) {
		return false
	}
	boundary := candidate[len(wanted)]
	if boundary != ' ' && boundary != '.' && boundary != ':' {
		return false
	}
	words := strings.Fields(candidate[len(wanted)+1:])
	if len(words) != 1 {
		return false
	}
	word := strings.ToLower(words[0])
	if len(newznab.SigWords(word)) != 1 || isEditionSuffix([]string{word}) || romanRe.MatchString(strings.ToUpper(word)) {
		return false
	}
	return !knownNarratorRole(word) && word != "retail" && word != "repack" && word != "epub" && word != "mobi" &&
		word != "azw3" && word != "m4b" && word != "mp3" && word != "pdf" &&
		!seriesWords[word] && !readMarkers[word]
}

// releaseNamesLongerWork needs an author credit, an author field, or an
// indexer's explicit book-title field. A bare string containing the requested
// word is not enough to condemn an ambiguous release.
func releaseNamesLongerWork(r newznab.SearchResult, wanted, author string, aliases []string) bool {
	if r.BookTitle != "" && ObviousLongerWorkTitle(wanted, r.BookTitle) {
		return true
	}
	names := append([]string{author}, aliases...)
	if ReleaseExplicitlyNamesLongerWork(wanted, r.Title, names) {
		return true
	}
	for _, name := range names {
		if name != "" && strings.EqualFold(strings.TrimSpace(r.Author), name) && ObviousLongerWorkTitle(wanted, r.Title) {
			return true
		}
	}
	return false
}

// ReleaseExplicitlyNamesLongerWork checks an author/work credit in a release
// name before any format parser can split a following narrator off as author.
func ReleaseExplicitlyNamesLongerWork(wanted, release string, authors []string) bool {
	for _, name := range authors {
		if name == "" {
			continue
		}
		if work, ok := creditedWorkTitle(release, name); ok && ObviousLongerWorkTitle(wanted, work) {
			return true
		}
	}
	return false
}

// A narrator's "by" inside a label is not an author/work credit. A second
// "by" after the closing bracket can still credit the book's actual author.
func byInsideNarratorLabel(release string, beforeBy int) bool {
	prefix := release[:beforeBy]
	for _, pair := range [][2]byte{{'(', ')'}, {'[', ']'}} {
		start := strings.LastIndexByte(prefix, pair[0])
		if start <= strings.LastIndexByte(prefix, pair[1]) {
			continue
		}
		label := strings.ToLower(strings.TrimSpace(prefix[start+1:]))
		if knownNarratorRole(label) {
			return true
		}
	}
	return false
}

func creditedWorkTitle(release, author string) (string, bool) {
	release = strings.TrimSpace(release)
	for _, sep := range []string{" - ", " – ", " — "} {
		prefix := author + sep
		if len(release) >= len(prefix) && strings.EqualFold(release[:len(prefix)], prefix) {
			return strings.TrimSpace(release[len(prefix):]), true
		}
	}
	lower := strings.ToLower(release)
	for i := strings.Index(lower, " by "); i > 0; {
		credit := strings.TrimSpace(release[i+4:])
		if !byInsideNarratorLabel(release, i) && len(credit) >= len(author) &&
			strings.EqualFold(credit[:len(author)], author) &&
			(len(credit) == len(author) || !unicode.IsLetter(rune(credit[len(author)]))) {
			return strings.TrimSpace(release[:i]), true
		}
		next := strings.Index(lower[i+4:], " by ")
		if next < 0 {
			break
		}
		i += 4 + next
	}
	// Preserve brackets in a scene title: normalizing before separating the
	// work from its author would hide a terminal edition label on another book.
	normAuthor := NormalizeRelease(author)
	for _, loc := range byCreditMarker.FindAllStringIndex(release, -1) {
		credit := NormalizeRelease(release[loc[1]:])
		if !byInsideNarratorLabel(release, loc[0]) &&
			(credit == normAuthor || strings.HasPrefix(credit, normAuthor+" ")) {
			return strings.TrimSpace(release[:loc[0]]), true
		}
	}
	// A dash after an author still marks an author-first credit.
	normRelease := NormalizeRelease(languageFormatTail.ReplaceAllString(release, ""))
	authorWords := strings.Fields(normAuthor)
	if toks, ok := tokenizeRelease(release); ok && len(authorWords) > 0 && len(toks) > len(authorWords) &&
		strings.HasPrefix(normRelease, normAuthor+" ") && strings.ContainsAny(toks[len(authorWords)].sep, "-–—") {
		return strings.TrimSpace(strings.TrimPrefix(normRelease, normAuthor)), true
	}
	needle := " by " + normAuthor
	if normAuthor != "" {
		for i := strings.Index(normRelease, needle); i > 0; {
			end := i + len(needle)
			if end == len(normRelease) || normRelease[end] == ' ' {
				return strings.TrimSpace(normRelease[:i]), true
			}
			next := strings.Index(normRelease[i+4:], needle)
			if next < 0 {
				break
			}
			i += 4 + next
		}
	}
	return "", false
}
