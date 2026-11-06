package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/vavallee/bindery/internal/models"
)

// checkedVariantSource carries each reviewed file's identity and language
// evidence into private staging. It is never a claim based on the book title.
type checkedVariantSource struct {
	path             string
	assessed         os.FileInfo
	verification     string
	detectedLanguage string
	adminOverride    bool
	auditEventID     int64
}

// A successful hardlink must still identify the regular file whose evidence
// was read. A new inode or a write to the same inode invalidates that review.
func sameAssessedSource(assessed, linked os.FileInfo) bool {
	return assessed != nil && linked != nil && linked.Mode().IsRegular() &&
		os.SameFile(assessed, linked) && assessed.Size() == linked.Size() &&
		assessed.ModTime().Equal(linked.ModTime())
}

func variantSourceChanged(phase, source string, cause error) error {
	if cause != nil {
		return fmt.Errorf("variant source %s %s: %w", source, phase, cause)
	}
	return fmt.Errorf("variant source %s %s (inode, size or mtime mismatch)", source, phase)
}

// reviewVariantImport reads source evidence and persists review decisions
// before placement. It never trusts a caller-supplied variant: the download
// and selected variant are reloaded from the database. The selected files
// remain private until the placement code verifies and promotes them.
func (s *Scanner) reviewVariantImport(ctx context.Context, dl *models.Download, book *models.Book, author *models.Author,
	format, downloadPath string, files []string) ([]checkedVariantSource, bool, string, error) {
	if s.books == nil || s.downloads == nil || s.history == nil || dl == nil || book == nil {
		return nil, false, "", fmt.Errorf("variant import requires persisted download, book and history")
	}
	stored, err := s.downloads.GetByID(ctx, dl.ID)
	if err != nil || stored == nil {
		return nil, false, "", fmt.Errorf("load variant download %d: %w", dl.ID, missingVariantRow(err))
	}
	if stored.VariantID == nil || stored.BookID == nil || *stored.BookID != book.ID || dl.BookID == nil || *dl.BookID != book.ID ||
		dl.VariantID == nil || *dl.VariantID != *stored.VariantID {
		return nil, false, "", fmt.Errorf("variant download/book association changed")
	}
	variants, err := s.books.VariantRepo().ListByBook(ctx, book.ID)
	if err != nil {
		return nil, false, "", fmt.Errorf("load intended variant: %w", err)
	}
	var variant *models.BookVariant
	for i := range variants {
		if variants[i].ID == *stored.VariantID {
			variant = &variants[i]
			break
		}
	}
	if variant == nil || variant.MediaType != format {
		return nil, false, "", fmt.Errorf("selected variant does not belong to book and downloaded format")
	}
	rejections := s.books.RejectionRepo()
	paths, err := rejections.ListVariantSourcePaths(ctx, book.ID, variant.ID)
	if err != nil {
		return nil, false, "", err
	}
	eligible := make([]string, 0, len(files))
	rejectedCount := 0
	for _, source := range files {
		if err := s.safeVariantSource(downloadPath, source); err != nil {
			return nil, false, "", err
		}
		// EPUB rejections name exact files; only an audiobook's rejected
		// content folder also covers its contained tracks.
		rejected, err := rejections.IsRejected(ctx, book.ID, variant.ID, source)
		if err != nil {
			return nil, false, "", err
		}
		if !rejected && variant.MediaType == models.MediaTypeAudiobook {
			for _, prior := range paths {
				if prior == source {
					rejected = true
					break
				}
				info, err := os.Lstat(prior)
				if err != nil {
					return nil, false, "", fmt.Errorf("inspect rejected source %s: %w", prior, err)
				}
				if info.Mode()&os.ModeSymlink != 0 {
					return nil, false, "", fmt.Errorf("rejected source is a symlink: %s", prior)
				}
				if info.IsDir() && strictlyWithin(source, prior) {
					if err := s.safeVariantSource(prior, source); err != nil {
						return nil, false, "", err
					}
					rejected = true
					break
				}
			}
		}
		if rejected {
			rejectedCount++
			continue
		}
		eligible = append(eligible, source)
	}
	if len(eligible) == 0 && rejectedCount > 0 {
		if err := s.books.VariantRepo().SetStatus(ctx, variant.ID, models.VariantReview); err != nil {
			return nil, false, "", fmt.Errorf("keep rejected variant in review: %w", err)
		}
		return nil, true, "source previously rejected for this book", nil
	}
	// Use the existing history API, not an unaudited boolean on the release.
	// Even a malformed or unrelated event must not waive a language mismatch.
	audit, err := s.history.ListByBook(ctx, book.ID)
	if err != nil {
		return nil, false, "", fmt.Errorf("load variant override audit: %w", err)
	}
	var override bool
	var auditEventID int64
	for _, event := range audit {
		if event.EventType != "variantLanguageOverride" {
			continue
		}
		var data struct {
			ActorUserID int64  `json:"actorUserId"`
			VariantID   int64  `json:"variantId"`
			DownloadID  int64  `json:"downloadId"`
			Reason      string `json:"reason"`
		}
		if json.Unmarshal([]byte(event.Data), &data) == nil && data.ActorUserID > 0 &&
			data.VariantID == variant.ID && data.DownloadID == stored.ID && strings.TrimSpace(data.Reason) != "" {
			override = true
			auditEventID = event.ID
			break
		}
	}
	var matching, uncertain, mismatched []checkedVariantSource
	var wrongWork bool
	var reason string
	for _, source := range eligible {
		// A manual formatHint cannot turn an M4B into an ebook. Likewise a
		// PDF sidecar alongside an audiobook is not an audio file to link.
		if detectDownloadFormat([]string{source}) != variant.MediaType {
			continue
		}
		assessed, err := os.Lstat(source)
		if err != nil {
			return nil, false, "", fmt.Errorf("inspect variant evidence source %s: %w", source, err)
		}
		if !assessed.Mode().IsRegular() {
			return nil, false, "", fmt.Errorf("variant evidence source is not regular: %s", source)
		}
		e, err := s.AssessVariantEvidence(ctx, stored, book, author, variant, []string{source}, override)
		if err != nil {
			return nil, false, "", err
		}
		if err := s.safeVariantSource(downloadPath, source); err != nil {
			return nil, false, "", err
		}
		current, err := os.Lstat(source)
		if err != nil || !sameAssessedSource(assessed, current) {
			return nil, false, "", variantSourceChanged("changed during assessment", source, err)
		}
		if e.WrongWork {
			wrongWork = true
			reason = e.Reason
		}
		candidate := checkedVariantSource{path: source, assessed: assessed, verification: "unverified", detectedLanguage: e.DetectedLanguage, adminOverride: override, auditEventID: auditEventID}
		switch {
		case e.WrongLanguage:
			mismatched = append(mismatched, candidate)
			if reason == "" {
				reason = e.Reason
			}
		case e.Verification == "verified" && e.DetectedLanguage == variant.Language:
			candidate.verification = "verified"
			candidate.detectedLanguage = e.DetectedLanguage
			matching = append(matching, candidate)
		default:
			uncertain = append(uncertain, candidate)
		}
	}
	if len(matching)+len(uncertain)+len(mismatched) == 0 {
		if rejectedCount > 0 {
			if err := s.books.VariantRepo().SetStatus(ctx, variant.ID, models.VariantReview); err != nil {
				return nil, false, "", fmt.Errorf("keep rejected variant in review: %w", err)
			}
			return nil, true, "source previously rejected for this book", nil
		}
		return nil, false, "", fmt.Errorf("download has no files of the requested variant format")
	}
	if wrongWork || len(matching)+len(uncertain) == 0 {
		if reason == "" {
			reason = "source evidence does not match the requested variant"
		}
		// A release that names another work is refused as a whole; an all-wrong
		// language release records only its mismatched files. An audiobook's
		// own content folder is the identity the admin endpoint records too.
		bad := mismatched
		if wrongWork {
			bad = append(append([]checkedVariantSource(nil), matching...), uncertain...)
			bad = append(bad, mismatched...)
		}
		for _, item := range bad {
			identity := variantRejectionIdentity(ctx, stored, downloadPath, item.path, variant.MediaType)
			if err := rejections.Record(ctx, book.ID, variant.ID, stored.ID, identity, reason); err != nil {
				return nil, false, "", fmt.Errorf("record rejected source: %w", err)
			}
		}
		if err := s.books.VariantRepo().SetStatus(ctx, variant.ID, models.VariantReview); err != nil {
			return nil, false, "", fmt.Errorf("mark variant for review: %w", err)
		}
		return nil, true, reason, nil
	}
	// Reject confident language mismatches by exact file even when an eligible
	// sibling remains. A retry can then skip the wrong file without holding
	// the whole download or a different language variant in review.
	for _, item := range mismatched {
		identity := variantRejectionIdentity(ctx, stored, downloadPath, item.path, variant.MediaType)
		if err := rejections.Record(ctx, book.ID, variant.ID, stored.ID, identity, reason); err != nil {
			return nil, false, "", fmt.Errorf("record rejected source: %w", err)
		}
	}
	// If one file is confidently the requested copy, an uncertain sibling
	// cannot take its place or claim a second file slot. Without a verified
	// match, the selected source stays unverified in its book-file row.
	selected := uncertain
	if len(matching) > 0 {
		selected = matching
	}
	return selected, false, "", nil
}

func missingVariantRow(err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("download row not found")
}

// strictlyWithin checks path segments, not a string prefix (a rejected
// /downloads/book must not match /downloads/book-other).
func strictlyWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// safeVariantSource checks each component from a configured download root (or
// the host's temp directory for test downloads). This also catches symlinks
// ABOVE a torrent content path reported as a single file. Only ancestors of a
// trusted root are exempt: on macOS, /var itself is a system symlink.
func (s *Scanner) safeVariantSource(root, source string) error {
	return s.safeVariantPath(root, source, false)
}

func (s *Scanner) safeVariantPath(root, source string, allowFolder bool) error {
	if !filepath.IsAbs(root) || !filepath.IsAbs(source) || filepath.Clean(root) != root || filepath.Clean(source) != source ||
		(root != source && !strictlyWithin(source, root)) {
		return fmt.Errorf("variant source outside canonical download path: %s", source)
	}
	anchor := filepath.VolumeName(root) + string(filepath.Separator)
	for _, configured := range []string{s.downloadDir, s.audiobookDownloadDir, os.TempDir()} {
		configured = filepath.Clean(configured)
		if configured != "." && filepath.IsAbs(configured) &&
			(root == configured || strictlyWithin(root, configured)) && len(configured) > len(anchor) {
			anchor = configured
		}
	}
	path := anchor
	for {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect variant source %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("variant source crosses symlink: %s", path)
		}
		if path == source {
			if !info.Mode().IsRegular() && !(allowFolder && info.IsDir()) {
				return fmt.Errorf("variant source is not a regular file or content folder: %s", source)
			}
			return nil
		}
		rel, _ := filepath.Rel(path, source)
		component := strings.Split(rel, string(filepath.Separator))[0]
		path = filepath.Join(path, component)
	}
}

// Audiobook source-folder rejection is only safe when the content path is
// the torrent's own folder (or strictly nested below a reported shared root).
// Ebook rejections and single-file torrents always name the exact file.
func variantRejectionIdentity(ctx context.Context, dl *models.Download, downloadPath, source, format string) string {
	// Ebook rejection is always the exact file, even inside a torrent's own
	// folder. A sibling or another language variant may still be correct.
	if format == models.MediaTypeEbook || dl.Protocol != "torrent" {
		return source
	}
	info, err := os.Lstat(downloadPath)
	if err != nil || !info.IsDir() {
		return source
	}
	if isTorrentOwnFolder(ctx) && strictlyWithin(source, downloadPath) {
		return downloadPath
	}
	if strictlyWithin(filepath.Dir(source), downloadPath) {
		return filepath.Dir(source)
	}
	return source
}

const maxRejectedFolderEntries = 1024

// rejectedLibraryInode is called before each of scanLibrary's three book-file
// writes. A rejection is scoped to the candidate book, and compares device/
// inode rather than path spelling. No torrent tree is walked: only the exact
// recorded source or a bounded, symlink-free rejected content folder.
func (s *Scanner) rejectedLibraryInode(ctx context.Context, bookID int64, path string) (bool, error) {
	if s.books == nil {
		return false, fmt.Errorf("book repository unavailable for scan rejection check")
	}
	paths, err := s.books.RejectionRepo().ListSourcePaths(ctx, bookID)
	if err != nil {
		return false, err
	}
	if len(paths) == 0 {
		return false, nil
	}
	candidate, err := os.Lstat(path)
	if err != nil {
		return false, fmt.Errorf("inspect scanned file %s: %w", path, err)
	}
	if !candidate.Mode().IsRegular() {
		return false, fmt.Errorf("scanned file is not regular: %s", path)
	}
	for _, source := range paths {
		if !filepath.IsAbs(source) || filepath.Clean(source) != source {
			return false, fmt.Errorf("invalid rejected source path")
		}
		if err := s.safeVariantPath(source, source, true); err != nil {
			return false, err
		}
		info, err := os.Lstat(source)
		if err != nil {
			return false, fmt.Errorf("inspect rejected source %s: %w", source, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("rejected source is symlinked: %s", source)
		}
		if info.Mode().IsRegular() {
			if os.SameFile(info, candidate) {
				return true, nil
			}
			continue
		}
		if !info.IsDir() {
			return false, fmt.Errorf("rejected source is not regular or directory: %s", source)
		}
		count := 0
		matched := false
		err = filepath.WalkDir(source, func(p string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			count++
			if count > maxRejectedFolderEntries {
				return fmt.Errorf("rejected folder exceeds %d entries", maxRejectedFolderEntries)
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("rejected folder contains symlink")
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			stat, err := os.Lstat(p)
			if err != nil {
				return err
			}
			if os.SameFile(stat, candidate) {
				matched = true
				return fs.SkipAll
			}
			return nil
		})
		if err != nil {
			return false, fmt.Errorf("inspect rejected folder %s: %w", source, err)
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}
