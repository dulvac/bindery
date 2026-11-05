package importer

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/vavallee/bindery/internal/indexer"
	"github.com/vavallee/bindery/internal/models"
)

// VariantEvidence is a read-only decision about a completed download, before
// any of its source files are linked into a requested language variant.
// DetectedLanguage is file evidence, never the book's catalogue language or a
// release-name guess. A review decision takes precedence over Verification.
type VariantEvidence struct {
	DetectedLanguage string
	Verification     string
	WrongWork        bool
	WrongLanguage    bool
	Reason           string
}

// AssessVariantEvidence reads only bounded source metadata and EPUB text. The
// caller must verify that hasExactAdminLanguageOverride was recorded by a named
// admin for this download's ID; this method never looks up or writes the audit.
// An override waives only WrongLanguage, not WrongWork or a false language tag.
// No client, file-placement or database mutation is performed here.
func (s *Scanner) AssessVariantEvidence(ctx context.Context, download *models.Download, book *models.Book, author *models.Author,
	variant *models.BookVariant, sourceFiles []string, hasExactAdminLanguageOverride bool) (VariantEvidence, error) {
	e := VariantEvidence{Verification: "unverified"}
	if download == nil || book == nil || variant == nil || variant.BookID != book.ID ||
		(variant.MediaType != models.MediaTypeEbook && variant.MediaType != models.MediaTypeAudiobook) ||
		!slices.Contains([]string{"eng", "rum", "fre"}, variant.Language) {
		return e, fmt.Errorf("variant evidence: download, book and canonical variant for this book are required")
	}
	if err := ctx.Err(); err != nil {
		return e, err
	}

	// A release marker can reject a known mismatch, but it can never verify a
	// copy: neither a release name nor an EPUB's OPF proves the text's language.
	reported := variantReleaseLanguages(download.Title, book.Title)
	if len(reported) == 1 && reported[0] != variant.Language {
		e.WrongLanguage = true
		e.Reason = "release language " + reported[0] + " differs from requested " + variant.Language
	} else if len(reported) > 1 {
		e.Reason = "release language is ambiguous"
	}

	// An explicit title AND conflicting author are stronger work evidence than
	// title overlap (e.g. "Dreamer of Dune" contains the word "Dune").
	names := s.authorNamesFor(ctx, author)
	parsed := ParseFilename(strings.ReplaceAll(download.Title, "/", " "))
	releaseAuthorAgrees, _ := ebookAuthorAgreement(names, parsed.Author)
	if authorAndWorkDisagree(book.Title, names, parsed.Title, parsed.Author) {
		e.WrongWork = true
		e.Reason = "release identifies a different work and author"
	}

	switch variant.MediaType {
	case models.MediaTypeEbook:
		s.assessEbookVariant(ctx, book, author, variant.Language, sourceFiles, &e)
	case models.MediaTypeAudiobook:
		releaseTitleDiffers := usableWorkTitle(parsed.Title) && !equivalentWorkTitle(book.Title, parsed.Title)
		s.assessAudiobookVariant(ctx, book, names, releaseTitleDiffers, !releaseAuthorAgrees, sourceFiles, &e)
	}
	if err := ctx.Err(); err != nil {
		return VariantEvidence{Verification: "unverified"}, err
	}
	if e.WrongWork {
		e.Verification = "unverified"
		e.Reason = "download identifies a different work"
		return e, nil
	}
	if e.WrongLanguage && hasExactAdminLanguageOverride && download.ID > 0 {
		e.WrongLanguage = false
		e.Reason = "language mismatch accepted for this download by named admin override"
	}
	if e.WrongLanguage {
		e.Verification = "unverified"
	}
	return e, nil
}

// A format-qualified MAM marker is more specific than a language word in the
// title itself (e.g. a book called "English Garden" marked [ROM / EPUB]). Use
// the existing indexer parser on those isolated markers so ordinary title
// words cannot create a spurious second language.
var variantFormatMarker = regexp.MustCompile(`(?i)\[[a-z]{2,3}\s*/\s*(?:EPUB|AZW3|MOBI|PDF|M4B|MP3|FLAC)\]`)
var variantLanguageLabel = regexp.MustCompile(`(?i)\[\s*([a-z]+)\s*\]`)

func variantReleaseLanguages(title, bookTitle string) []string {
	markers := variantFormatMarker.FindAllString(title, -1)
	// A single language word in brackets is an edition label even when that
	// word also appears in the catalogue title. Prose inside brackets is not.
	var explicit []string
	for _, label := range variantLanguageLabel.FindAllStringSubmatch(title, -1) {
		for _, code := range indexer.ReleaseLanguages(label[1]) {
			if !slices.Contains(explicit, code) {
				explicit = append(explicit, code)
			}
		}
	}
	if len(markers) == 0 {
		// An unmarked word already present in the catalogue title is not
		// an edition label. Keep independently bracketed labels.
		codes := indexer.ReleaseLanguages(title)
		bookCodes := indexer.ReleaseLanguages(bookTitle)
		return slices.DeleteFunc(codes, func(code string) bool {
			return slices.Contains(bookCodes, code) && !slices.Contains(explicit, code)
		})
	}
	codes := explicit
	for _, marker := range markers {
		for _, code := range indexer.ReleaseLanguages(marker) {
			if !slices.Contains(codes, code) {
				codes = append(codes, code)
			}
		}
	}
	return codes
}

// authorAndWorkDisagree shares the content checks' author comparison but
// demands an equal title form, not the broad whole-word containment that lets
// "Dreamer of Dune" support a search for the novel "Dune".
func authorAndWorkDisagree(wanted string, names []string, title, creditedAuthor string) bool {
	if len(names) == 0 || !usableWorkTitle(title) || equivalentWorkTitle(wanted, title) {
		return false
	}
	agrees, _ := ebookAuthorAgreement(names, creditedAuthor)
	return !agrees
}

func equivalentWorkTitle(wanted, found string) bool {
	if otherVolumeOfSameWork(wanted, found) {
		return false
	}
	for _, a := range contentTitleForms(wanted) {
		for _, b := range contentTitleForms(found) {
			if a == b {
				return true
			}
		}
	}
	return false
}

func (s *Scanner) assessEbookVariant(ctx context.Context, book *models.Book, author *models.Author, requested string, files []string, e *VariantEvidence) {
	// Never let an unbounded ZIP directory reach the older OPF metadata reader.
	// Kindle's record reader is already capped, but has no reliable text signal.
	bounded := make([]string, 0, min(len(files), maxContentCheckEpubReads))
	var epubs []string
	var candidates int
	for _, path := range files {
		if !HasEmbeddedMetadata(path) {
			continue
		}
		candidates++
		if candidates > maxContentCheckEpubReads {
			if e.Reason == "" {
				e.Reason = "too many ebooks to assess safely"
			}
			return
		}
		if IsEpubFile(path) {
			if err := checkTextZIPDirectory(path); err != nil {
				if e.Reason == "" {
					e.Reason = "ebook metadata or text could not be read"
				}
				continue
			}
			epubs = append(epubs, path)
		}
		bounded = append(bounded, path)
	}
	work := s.checkEbookContentMode(ctx, book, author, bounded, contentCheckBlock)
	if work.flagged && !work.uncertain {
		e.WrongWork = true
		e.Reason = "ebook work metadata conflicts with the requested book"
	} else if len(bounded) > 0 && !bookIsCompilationBy(book.Title, authorName(author)) {
		// The legacy check allows whole-word title containment even when an
		// OPF explicitly credits a different author. For variant import, all
		// readable files must instead support the same work, not a biography
		// whose longer title happens to contain the novel's short title.
		names := s.authorNamesFor(ctx, author)
		allDifferent := true
		for _, path := range bounded {
			meta, err := ReadEbookMetadata(path)
			if err != nil || !authorAndWorkDisagree(book.Title, names, meta.Title, meta.Author) {
				allDifferent = false
				break
			}
		}
		if allDifferent {
			e.WrongWork = true
			e.Reason = "ebook work metadata names a different author and title"
		}
	}
	// A single reliable file can prove its own language; unknown/mixed files
	// never become English just because their OPF or release name says en.
	// Two different confident languages in one release must not be assigned
	// wholesale to either variant. File selection belongs to Task 7D2.
	var detected string
	allConfident := len(epubs) > 0 && len(epubs) == len(files)
	for _, path := range epubs {
		if ctx.Err() != nil {
			return
		}
		language, confident, err := DetectEbookTextLanguage(path)
		if err != nil || !confident {
			allConfident = false
			continue
		}
		if detected != "" && detected != language {
			if e.Reason == "" {
				e.Reason = "ebook files contain multiple detected languages"
			}
			e.DetectedLanguage = ""
			return
		}
		detected = language
	}
	e.DetectedLanguage = detected
	if detected != "" && detected != requested {
		e.WrongLanguage = true
		e.Reason = "ebook text language " + detected + " differs from requested " + requested
	} else if allConfident && detected == requested {
		e.Verification = "verified"
	}
}

func (s *Scanner) assessAudiobookVariant(ctx context.Context, book *models.Book, names []string, releaseTitleDiffers, releaseAuthorConflict bool, files []string, e *VariantEvidence) {
	var audio []string
	for _, path := range files {
		if IsAudioTagFile(path) {
			audio = append(audio, path)
		}
	}
	if len(audio) == 0 {
		return
	}
	// Reuse the existing check's sampling and wall-clock budget. An incomplete
	// read cannot confidently condemn a whole release on the tags alone.
	sampled := len(audio) > maxContentCheckTagReads
	if sampled {
		audio = sampleEvenly(audio, maxContentCheckTagReads)
	}
	budget, cancel := context.WithTimeout(ctx, contentCheckBudget)
	defer cancel()
	tags := make([]AudioTags, 0, len(audio))
	for _, path := range audio {
		if budget.Err() != nil {
			break
		}
		tag, err := s.audioTags(path)
		if err != nil {
			if e.Reason == "" {
				e.Reason = "audiobook tags could not be read"
			}
			continue
		}
		tags = append(tags, tag)
	}
	if budget.Err() != nil || sampled || len(tags) != len(audio) {
		return
	}
	res := evaluateAudiobookContent(book.Title, tags)
	if !bookIsCompilation(book.Title) && (res.flagged && (res.conflicts >= 2 || audioTagsIncludeDifferentWork(book.Title, names, tags)) ||
		audioTagsNameAnotherWork(book.Title, names, tags) ||
		(releaseTitleDiffers || releaseAuthorConflict) && audioTagAuthorsConfirmConflict(names, tags)) {
		e.WrongWork = true
		e.Reason = "audiobook work tags conflict with the requested book"
	}
}

// Mixed album titles need an independently conflicting author before they
// can identify another work; different titles by the same author may be retitles.
func audioTagsIncludeDifferentWork(wanted string, names []string, tags []AudioTags) bool {
	for _, tag := range tags {
		album := strings.TrimSpace(tag.Album)
		if usableWorkTitle(album) && !tagNamesNoWork(tag, album) &&
			!equivalentWorkTitle(wanted, album) && audioTagAuthorsDisagree(names, tag) {
			return true
		}
	}
	return false
}

func audioTagsNameAnotherWork(wanted string, names []string, tags []AudioTags) bool {
	var differing bool
	for _, tag := range tags {
		album := strings.TrimSpace(tag.Album)
		if !usableWorkTitle(album) || tagNamesNoWork(tag, album) {
			continue
		}
		if equivalentWorkTitle(wanted, album) {
			return false // at least one file names the requested work
		}
		if !audioTagAuthorsDisagree(names, tag) {
			return false // a translation or retitle by the right author
		}
		differing = true
	}
	return differing
}

// A distinct release title or wrong release author, corroborated by the file's
// wrong author, identifies a work conflict even if its album names the wanted
// book. A matching or missing author tag prevents that conclusion.
func audioTagAuthorsConfirmConflict(names []string, tags []AudioTags) bool {
	if len(tags) == 0 {
		return false
	}
	for _, tag := range tags {
		if !audioTagAuthorsDisagree(names, tag) {
			return false
		}
	}
	return true
}

func audioTagAuthorsDisagree(names []string, tag AudioTags) bool {
	if len(names) == 0 {
		return false
	}
	var named bool
	for _, credit := range []string{tag.Author, tag.Artist, tag.AlbumArtist, tag.Composer} {
		if isNarratorCredit(credit) || contributorRoleRe.MatchString(credit) {
			continue
		}
		if loc := authorRoleRe.FindStringIndex(credit); loc != nil {
			credit = strings.TrimSpace(credit[:loc[0]])
		}
		if strings.TrimSpace(credit) == "" {
			continue
		}
		agrees, _ := ebookAuthorAgreement(names, credit)
		if agrees {
			return false
		}
		named = true
	}
	return named
}
