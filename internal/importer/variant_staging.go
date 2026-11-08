//go:build linux || darwin

package importer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

// A stagedVariantFile is never publicly visible until its source has been
// checked against the exact inode, size and mtime read during evidence review.
type stagedVariantFile struct {
	checked checkedVariantSource
	variant models.BookVariant
	dest    string
	stage   string // basename in the private attempt, or path below its audio folder
}

// These hooks are set only by race tests. Production leaves them nil; actual
// promotion always uses the kernel's atomic no-replace operation.
var variantPromotionHook func()
var variantBeforeTrackHook func()
var variantCommitWindowHook func()
var variantAfterAttemptHook func(string)

// Resolve the private destination relative to the verified attempt fd.
// Tests inject source mutations at this edge without changing the source API.
var variantStageLink = func(source string, parentFD int, name string) error {
	return unix.Linkat(unix.AT_FDCWD, source, parentFD, name, 0)
}

func (s *Scanner) importTorrentVariant(ctx context.Context, dl *models.Download, book *models.Book, author *models.Author,
	format, downloadPath string, files []string, selected []checkedVariantSource) error {
	if dl.Protocol != "torrent" || dl.VariantID == nil || book == nil || s.books == nil ||
		!filepath.IsAbs(s.libraryDir) || filepath.Base(s.libraryDir) != "books" || filepath.Clean(s.libraryDir) != s.libraryDir {
		return fmt.Errorf("variant placement requires torrent, persisted variant and a canonical books root")
	}
	variants, err := s.books.VariantRepo().ListByBook(ctx, book.ID)
	if err != nil {
		return fmt.Errorf("load placement variants: %w", err)
	}
	var wanted *models.BookVariant
	for i := range variants {
		if variants[i].ID == *dl.VariantID {
			wanted = &variants[i]
			break
		}
	}
	if wanted == nil || wanted.MediaType != format ||
		wanted.Status == models.VariantReview && !slices.ContainsFunc(selected, func(p checkedVariantSource) bool { return p.adminOverride }) {
		return fmt.Errorf("variant is missing, changed, or in review; explicit admin retry or wanted transition required")
	}
	if len(selected) == 0 {
		return fmt.Errorf("no assessed variant files")
	}

	// Other requested ebook languages may share a release. Their files must
	// independently prove their text language; a release label is not evidence
	// for a sibling, and an unknown file cannot claim another variant's slot.
	chosen := make([]stagedVariantFile, 0, len(selected))
	for _, source := range selected {
		chosen = append(chosen, stagedVariantFile{checked: source, variant: *wanted})
	}
	if format == models.MediaTypeEbook {
		for _, variant := range variants {
			if variant.ID == wanted.ID || variant.MediaType != format ||
				(variant.Status != models.VariantWanted && variant.Status != models.VariantDownloading) {
				continue
			}
			for _, source := range files {
				if detectDownloadFormat([]string{source}) != format || slices.ContainsFunc(chosen, func(p stagedVariantFile) bool { return p.checked.path == source }) {
					continue
				}
				if err := s.safeVariantSource(downloadPath, source); err != nil {
					return err
				}
				rejected, err := s.books.RejectionRepo().IsRejected(ctx, book.ID, variant.ID, source)
				if err != nil {
					return err
				}
				if rejected {
					continue
				}
				before, err := os.Lstat(source)
				if err != nil || !before.Mode().IsRegular() {
					return variantSourceChanged("before sibling review", source, err)
				}
				// Only for independently verified bundle members: the grab's
				// language marker describes its primary file, not every EPUB.
				neutral := *dl
				neutral.Title = variantFormatMarker.ReplaceAllString(dl.Title, "")
				e, err := s.AssessVariantEvidence(ctx, &neutral, book, author, &variant, []string{source}, false)
				if err != nil {
					return err
				}
				after, err := os.Lstat(source)
				if err != nil || !sameAssessedSource(before, after) {
					return variantSourceChanged("during sibling review", source, err)
				}
				if !e.WrongWork && !e.WrongLanguage && e.Verification == "verified" && e.DetectedLanguage == variant.Language {
					chosen = append(chosen, stagedVariantFile{checked: checkedVariantSource{path: source, assessed: before, verification: "verified", detectedLanguage: e.DetectedLanguage}, variant: variant})
				}
			}
		}
		if slices.ContainsFunc(chosen, func(p stagedVariantFile) bool { return p.checked.verification == "verified" }) {
			chosen = slices.DeleteFunc(chosen, func(p stagedVariantFile) bool { return p.checked.verification != "verified" })
		}
	}
	if len(chosen) == 0 {
		return fmt.Errorf("no confidently assigned files after variant review")
	}

	var root string
	if format == models.MediaTypeAudiobook {
		root = s.effectiveAudiobookDir(ctx, author)
	} else {
		root = s.effectiveLibraryDir(ctx, author)
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("noncanonical variant library root %q", root)
	}
	seriesTitle, seriesNum := s.primarySeriesFor(ctx, book)
	renamer := s.destRenamer(ctx)
	if format == models.MediaTypeAudiobook {
		base, err := renamer.AudiobookDestDir(root, author, book, seriesTitle, seriesNum)
		if err != nil {
			return err
		}
		dest, err := VariantDestination(base, wanted.Language, true)
		if err != nil {
			return err
		}
		for i := range chosen {
			chosen[i].dest = dest
		}
	} else {
		for i := range chosen {
			base, err := renamer.DestPath(root, author, book, seriesTitle, seriesNum, chosen[i].checked.path)
			if err != nil {
				return err
			}
			chosen[i].dest, err = VariantDestination(base, chosen[i].variant.Language, false)
			if err != nil {
				return err
			}
		}
	}
	// The public audio folder preserves only selected tracks and their
	// relative disc subfolders, never unrelated torrent-root siblings.
	if format == models.MediaTypeAudiobook {
		for i := range chosen {
			base := downloadPath
			if !strictlyWithin(chosen[i].checked.path, base) {
				base = filepath.Dir(chosen[i].checked.path)
			}
			rel, err := filepath.Rel(base, chosen[i].checked.path)
			if err != nil || rel == "." || !safeVariantRelative(rel) {
				return fmt.Errorf("unsafe audio source name: %q", chosen[i].checked.path)
			}
			chosen[i].stage = filepath.Join("audio", rel)
		}
	}
	// No named public object may already exist, including a symlink, and
	// distinct selected files must not resolve to the same ebook path.
	seen := map[string]bool{}
	for _, p := range chosen {
		if _, err := ensureContained(p.dest, root); err != nil {
			return fmt.Errorf("variant destination outside library root: %q: %w", p.dest, err)
		}
		if p.dest == root {
			return fmt.Errorf("variant destination outside library root: %q", p.dest)
		}
		if seen[p.dest] && format == models.MediaTypeEbook {
			return fmt.Errorf("variant destination collision: %s", p.dest)
		}
		seen[p.dest] = true
	}

	// An interrupted import may have committed the file/variant association
	// just before losing its download status. Only a *complete* on-disk,
	// same-inode and same-evidence association is an idempotent retry.
	if already, err := s.alreadyTrackedVariant(ctx, book, root, chosen); err != nil {
		return err
	} else if already {
		return nil
	}

	attempt, err := newVariantAttempt(filepath.Dir(s.libraryDir), root)
	if err != nil {
		return err
	}
	defer attempt.close()
	if variantAfterAttemptHook != nil {
		variantAfterAttemptHook(attempt.name)
	}
	if err := attempt.checkStageRoot(); err != nil {
		return err
	}
	if format == models.MediaTypeAudiobook {
		if err := unix.Mkdirat(attempt.attemptFD, "audio", 0o700); err != nil {
			return fmt.Errorf("stage audio folder: %w", err)
		}
	}
	for i := range chosen {
		p := &chosen[i]
		if err := s.safeVariantSource(downloadPath, p.checked.path); err != nil {
			return err
		}
		current, err := os.Lstat(p.checked.path)
		if err != nil || !sameAssessedSource(p.checked.assessed, current) {
			return variantSourceChanged("before staging", p.checked.path, err)
		}
		if format == models.MediaTypeAudiobook && current.Mode().Perm()&0o004 == 0 {
			return fmt.Errorf("audio source %s is not readable by ABS's separate UID; cannot chmod a hardlink without changing its torrent seed", p.checked.path)
		}
		if format != models.MediaTypeAudiobook {
			p.stage = fmt.Sprintf("file-%d", i)
		}
		if err := attempt.linkStage(p.checked.path, p.stage); err != nil {
			return fmt.Errorf("stage hardlink (no copy fallback) %s: %w", p.checked.path, err)
		}
		linked, err := statNoFollowRelative(attempt.attemptFD, p.stage)
		if err != nil || !sameAssessedSource(p.checked.assessed, linked) {
			return variantSourceChanged("at staged hardlink", p.checked.path, err)
		}
	}
	for _, p := range chosen {
		if err := s.safeVariantSource(downloadPath, p.checked.path); err != nil {
			return err
		}
		source, sourceErr := os.Lstat(p.checked.path)
		staged, stageErr := statNoFollowRelative(attempt.attemptFD, p.stage)
		if sourceErr != nil || stageErr != nil || !sameAssessedSource(p.checked.assessed, source) || !sameAssessedSource(p.checked.assessed, staged) {
			return variantSourceChanged("before promotion", p.checked.path, errors.Join(sourceErr, stageErr))
		}
	}
	if variantPromotionHook != nil {
		variantPromotionHook()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A source or staged inode changed by a writer during preflight must not
	// become visible. In particular, do this after the race-test boundary.
	for _, p := range chosen {
		source, sourceErr := os.Lstat(p.checked.path)
		staged, stageErr := statNoFollowRelative(attempt.attemptFD, p.stage)
		if sourceErr != nil || stageErr != nil || !sameAssessedSource(p.checked.assessed, source) || !sameAssessedSource(p.checked.assessed, staged) {
			return variantSourceChanged("immediately before promotion", p.checked.path, errors.Join(sourceErr, stageErr))
		}
		if format == models.MediaTypeAudiobook && source.Mode().Perm()&0o004 == 0 {
			return fmt.Errorf("audio source %s is no longer readable by ABS's separate UID", p.checked.path)
		}
	}
	// Keep review unchanged until successful placement. The exact audit and
	// expected status are checked again by TrackVariant in its transaction.
	var stagedFolder os.FileInfo
	if format == models.MediaTypeAudiobook {
		// The stage root and attempt remain 0700, but every directory that
		// will move into ABS's mount must allow a different UID to traverse.
		if err := attempt.makeAudioDirectoriesReadable(); err != nil {
			return fmt.Errorf("prepare audio folder for ABS: %w", err)
		}
		stagedFolder, err = statNoFollowRelative(attempt.attemptFD, "audio")
		if err != nil {
			return fmt.Errorf("staged audio directory changed: %w", err)
		}
		if !stagedFolder.IsDir() {
			return fmt.Errorf("staged audio directory changed: not a directory")
		}
	}
	if err := attempt.checkStageRoot(); err != nil {
		return err
	}
	// Folder promotion is a single atomic no-replace operation. Ebook links
	// are individually no-replace; never roll back public paths after success.
	if format == models.MediaTypeAudiobook {
		if err := attempt.promoteAudio(root, chosen[0].dest); err != nil {
			return err
		}
	} else {
		for i, p := range chosen {
			if err := attempt.promoteFile(root, p.stage, p.dest); err != nil {
				if i > 0 {
					return fmt.Errorf("stranded verified public link at %s; reconcile manually after partial promotion: %w", chosen[0].dest, err)
				}
				return err
			}
		}
	}
	if variantBeforeTrackHook != nil {
		variantBeforeTrackHook()
	}
	validate := func() error {
		return s.validatePromotedVariant(downloadPath, root, chosen, stagedFolder)
	}
	if err := validate(); err != nil {
		return err
	}
	claims := make([]db.VariantFileClaim, 0, len(chosen))
	for _, p := range chosen {
		approval := db.VariantReviewApproval{ExpectedStatus: p.variant.Status}
		if p.variant.Status == models.VariantReview {
			approval.AuditEventID, approval.DownloadID = p.checked.auditEventID, dl.ID
		}
		claims = append(claims, db.VariantFileClaim{
			VariantID: p.variant.ID, Format: format, Path: p.dest,
			Language: p.checked.detectedLanguage, Verification: p.checked.verification, Approval: approval,
		})
		if format == models.MediaTypeAudiobook {
			break // one folder, one association
		}
	}
	if err := s.books.TrackVariantFilesChecked(ctx, book.ID, claims, func() error {
		if variantCommitWindowHook != nil {
			variantCommitWindowHook()
		}
		return validate()
	}); err != nil {
		return fmt.Errorf("stranded verified public link(s), starting at %s; reconcile manually (no public rollback): %w", chosen[0].dest, err)
	}
	return nil
}

// A name on a public path is not a claim to its contents. Check the original
// assessed source and reopen the destination through no-follow directories
// for every selected file, including the folder's own inode for audio.
func (s *Scanner) validatePromotedVariant(downloadPath, root string, chosen []stagedVariantFile, stagedFolder os.FileInfo) error {
	for _, p := range chosen {
		if err := s.safeVariantSource(downloadPath, p.checked.path); err != nil {
			return fmt.Errorf("stranded public link at %s; unsafe source: %w", p.dest, err)
		}
		source, err := os.Lstat(p.checked.path)
		if err != nil || !sameAssessedSource(p.checked.assessed, source) {
			return fmt.Errorf("stranded public link at %s: %w", p.dest, variantSourceChanged("after promotion", p.checked.path, err))
		}
		if p.variant.MediaType == models.MediaTypeAudiobook && source.Mode().Perm()&0o004 == 0 {
			return fmt.Errorf("stranded public link at %s; audio source is no longer readable by ABS's separate UID", p.dest)
		}
		fd, name, err := openVariantDestination(root, p.dest)
		if err != nil {
			return fmt.Errorf("stranded public link at %s; unsafe destination: %w", p.dest, err)
		}
		var linked os.FileInfo
		if p.variant.MediaType == models.MediaTypeAudiobook {
			folder, openErr := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if openErr == nil {
				// #nosec G115 -- successful Openat returns a non-negative file descriptor.
				file := os.NewFile(uintptr(folder), name)
				folderInfo, statErr := file.Stat()
				if statErr != nil {
					openErr = fmt.Errorf("public directory changed before tracking: %w", statErr)
				} else if !os.SameFile(stagedFolder, folderInfo) {
					openErr = fmt.Errorf("public directory changed before tracking: inode mismatch")
				} else {
					linked, openErr = statNoFollowRelative(folder, strings.TrimPrefix(p.stage, "audio"+string(filepath.Separator)))
				}
				_ = file.Close()
			}
			err = openErr
		} else {
			linked, err = statNoFollowRelative(fd, name)
		}
		_ = unix.Close(fd)
		if err != nil {
			return fmt.Errorf("stranded public link at %s; destination changed before tracking: %w", p.dest, err)
		}
		if !sameAssessedSource(p.checked.assessed, linked) {
			return fmt.Errorf("stranded public link at %s; destination changed before tracking: inode or evidence mismatch", p.dest)
		}
	}
	return nil
}

// alreadyTrackedVariant handles only the narrow crash window after the DB
// transaction and before the download status update. An untracked path is
// never adopted and a partially tracked bundle requires manual reconciliation.
func (s *Scanner) alreadyTrackedVariant(ctx context.Context, book *models.Book, root string, chosen []stagedVariantFile) (bool, error) {
	rows, err := s.books.ListFiles(ctx, book.ID)
	if err != nil {
		return false, fmt.Errorf("load tracked variant files: %w", err)
	}
	matched := 0
	for _, p := range chosen {
		var row *models.BookFile
		for i := range rows {
			if rows[i].Path == p.dest {
				row = &rows[i]
				break
			}
		}
		if row == nil {
			continue
		}
		if row.VariantID == nil || *row.VariantID != p.variant.ID || row.Format != p.variant.MediaType ||
			row.Verification != p.checked.verification || row.Language != p.checked.detectedLanguage {
			return false, fmt.Errorf("existing variant path %s has a different DB claim", p.dest)
		}
		if err := s.safeVariantSource(p.checked.path, p.checked.path); err != nil {
			return false, err
		}
		current, err := os.Lstat(p.checked.path)
		if err != nil || !sameAssessedSource(p.checked.assessed, current) {
			return false, variantSourceChanged("during tracked retry", p.checked.path, err)
		}
		fd, name, err := openVariantDestination(root, p.dest)
		if err != nil {
			return false, err
		}
		var linked os.FileInfo
		if p.variant.MediaType == models.MediaTypeAudiobook {
			folder, openErr := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if openErr == nil {
				rel := strings.TrimPrefix(p.stage, "audio"+string(filepath.Separator))
				linked, openErr = statNoFollowRelative(folder, rel)
				_ = unix.Close(folder)
			}
			err = openErr
		} else {
			linked, err = statNoFollowRelative(fd, name)
		}
		_ = unix.Close(fd)
		if err != nil {
			return false, fmt.Errorf("tracked variant path %s no longer matches assessed source: %w", p.dest, err)
		}
		if !sameAssessedSource(p.checked.assessed, linked) {
			return false, fmt.Errorf("tracked variant path %s no longer matches assessed source: inode or evidence mismatch", p.dest)
		}
		matched++
	}
	if matched > 0 && matched != len(chosen) {
		return false, fmt.Errorf("partially tracked variant bundle needs manual reconciliation")
	}
	return matched == len(chosen), nil
}

func statNoFollowRelative(fd int, rel string) (os.FileInfo, error) {
	if !safeVariantRelative(rel) {
		return nil, fmt.Errorf("unsafe relative file %q", rel)
	}
	current, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, openErr := unix.Openat(current, part, flags, 0)
		_ = unix.Close(current)
		if openErr != nil {
			return nil, openErr
		}
		current = next
	}
	// #nosec G115 -- successful Dup or Openat returns a non-negative file descriptor.
	file := os.NewFile(uintptr(current), rel)
	defer file.Close()
	return file.Stat()
}

func safeVariantRelative(path string) bool {
	return path != "" && path != "." && !filepath.IsAbs(path) && filepath.Clean(path) == path &&
		path != ".." && !strings.HasPrefix(path, ".."+string(filepath.Separator))
}

type variantAttempt struct {
	path, name                   string // path is for diagnostics only, never for stage I/O
	parentFD, stageFD, attemptFD int
	stageIdentity                unix.Stat_t
}

// The stage is a private sibling of books/audiobooks, never inside either
// public ABS mount. Hold the verified root and random attempt by descriptor:
// reopening either by path would let a renamed root redirect staging/cleanup.
func newVariantAttempt(libraryParent, publicRoot string) (*variantAttempt, error) {
	if !filepath.IsAbs(libraryParent) || filepath.Clean(libraryParent) != libraryParent ||
		libraryParent == publicRoot || strictlyWithin(libraryParent, publicRoot) {
		return nil, fmt.Errorf("unsafe variant staging parent %q", libraryParent)
	}
	parent, err := unix.Open(libraryParent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open staging parent: %w", err)
	}
	defer func() {
		if parent >= 0 {
			_ = unix.Close(parent)
		}
	}()
	const stageName = ".bindery-staging"
	if err := unix.Mkdirat(parent, stageName, 0o700); err != nil && !errors.Is(err, syscall.EEXIST) {
		return nil, fmt.Errorf("create private staging root: %w", err)
	}
	fd, err := unix.Openat(parent, stageName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open private staging root: %w", err)
	}
	defer func() {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}()
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if int64(st.Uid) != int64(os.Geteuid()) || st.Mode&0o777 != 0o700 {
		return nil, fmt.Errorf("private staging root has wrong owner or mode")
	}
	var attemptName string
	for i := 0; i < 3; i++ {
		var id [16]byte
		if _, err = rand.Read(id[:]); err != nil {
			break
		}
		attemptName = hex.EncodeToString(id[:])
		err = unix.Mkdirat(fd, attemptName, 0o700)
		if !errors.Is(err, syscall.EEXIST) {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("create private attempt: %w", err)
	}
	attemptFD, err := unix.Openat(fd, attemptName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Unlinkat(fd, attemptName, unix.AT_REMOVEDIR)
		return nil, fmt.Errorf("open private attempt: %w", err)
	}
	a := &variantAttempt{
		path: filepath.Join(libraryParent, stageName, attemptName), name: attemptName,
		parentFD: parent, stageFD: fd, attemptFD: attemptFD, stageIdentity: st,
	}
	parent, fd = -1, -1
	return a, nil
}

func (a *variantAttempt) checkStageRoot() error {
	var st unix.Stat_t
	if err := unix.Fstatat(a.parentFD, ".bindery-staging", &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("private staging root changed during import: %w", err)
	}
	if st.Dev != a.stageIdentity.Dev || st.Ino != a.stageIdentity.Ino || st.Mode&unix.S_IFMT != unix.S_IFDIR ||
		int64(st.Uid) != int64(os.Geteuid()) || st.Mode&0o777 != 0o700 {
		return fmt.Errorf("private staging root changed during import: identity, owner or mode mismatch")
	}
	return nil
}

// stageParent returns a descriptor for rel's immediate parent. Intermediate
// folders are private, owned, no-follow directories even for nested discs.
func (a *variantAttempt) stageParent(rel string, create bool) (int, string, error) {
	if !safeVariantRelative(rel) {
		return -1, "", fmt.Errorf("unsafe private stage name %q", rel)
	}
	fd, err := unix.Dup(a.attemptFD)
	if err != nil {
		return -1, "", err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		if create {
			if err := unix.Mkdirat(fd, part, 0o700); err != nil && !errors.Is(err, syscall.EEXIST) {
				_ = unix.Close(fd)
				return -1, "", err
			}
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			return -1, "", openErr
		}
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func (a *variantAttempt) linkStage(source, rel string) error {
	if err := a.checkStageRoot(); err != nil {
		return err
	}
	fd, name, err := a.stageParent(rel, true)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return variantStageLink(source, fd, name)
}

func (a *variantAttempt) makeAudioDirectoriesReadable() error {
	if err := a.checkStageRoot(); err != nil {
		return err
	}
	folder, err := unix.Openat(a.attemptFD, "audio", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(folder) }()
	return makeVariantDirectoriesReadable(folder)
}

func makeVariantDirectoriesReadable(fd int) error {
	dup, err := unix.Dup(fd)
	if err != nil {
		return err
	}
	// #nosec G115 -- successful Dup returns a non-negative file descriptor.
	file := os.NewFile(uintptr(dup), "staged audio directory")
	entries, err := file.ReadDir(-1)
	_ = file.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in staged audio folder")
		}
		if !entry.IsDir() {
			continue
		}
		child, err := unix.Openat(fd, entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		err = makeVariantDirectoriesReadable(child)
		_ = unix.Close(child)
		if err != nil {
			return err
		}
	}
	return unix.Fchmod(fd, 0o755)
}

func (a *variantAttempt) close() {
	defer func() { _ = unix.Close(a.parentFD) }()
	defer func() { _ = unix.Close(a.stageFD) }()
	defer func() { _ = unix.Close(a.attemptFD) }()
	// Never clean a path the stage-root owner may have swapped underneath us.
	var held, named unix.Stat_t
	err := unix.Fstat(a.attemptFD, &held)
	if err == nil {
		err = unix.Fstatat(a.stageFD, a.name, &named, unix.AT_SYMLINK_NOFOLLOW)
	}
	if err == nil && (held.Dev != named.Dev || held.Ino != named.Ino || named.Mode&unix.S_IFMT != unix.S_IFDIR) {
		err = fmt.Errorf("private attempt name changed")
	}
	if err == nil {
		err = removeVariantStageContents(a.attemptFD)
	}
	if err == nil {
		err = unix.Unlinkat(a.stageFD, a.name, unix.AT_REMOVEDIR)
	}
	if err != nil {
		// A failed cleanup remains private and needs manual inspection.
		fmt.Fprintf(os.Stderr, "bindery: private variant staging cleanup %s: %v\n", a.path, err)
	}
}

func removeVariantStageContents(fd int) error {
	dup, err := unix.Dup(fd)
	if err != nil {
		return err
	}
	// #nosec G115 -- successful Dup returns a non-negative file descriptor.
	file := os.NewFile(uintptr(dup), "private variant attempt")
	entries, err := file.ReadDir(-1)
	_ = file.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			child, err := unix.Openat(fd, entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			err = removeVariantStageContents(child)
			_ = unix.Close(child)
			if err != nil {
				return err
			}
			if err := unix.Unlinkat(fd, entry.Name(), unix.AT_REMOVEDIR); err != nil {
				return err
			}
		} else if err := unix.Unlinkat(fd, entry.Name(), 0); err != nil {
			return err
		}
	}
	return nil
}

// openVariantDestination uses no-follow directory descriptors at every
// component. The final Linkat/Renameat2 cannot traverse a swapped parent or
// overwrite an entry another writer created after our collision preflight.
func openVariantDestination(root, dest string) (int, string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return -1, "", fmt.Errorf("invalid variant root %q", root)
	}
	if _, err := ensureContained(dest, root); err != nil || dest == root {
		return -1, "", fmt.Errorf("variant path outside root: %q", dest)
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fmt.Errorf("open variant library root: %w", err)
	}
	rel, _ := filepath.Rel(root, filepath.Dir(dest))
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if part == "" || part == "." || part == ".." {
				_ = unix.Close(fd)
				return -1, "", fmt.Errorf("invalid variant path component")
			}
			if err := unix.Mkdirat(fd, part, 0o755); err != nil && !errors.Is(err, syscall.EEXIST) {
				_ = unix.Close(fd)
				return -1, "", err
			}
			next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			_ = unix.Close(fd)
			if err != nil {
				return -1, "", fmt.Errorf("open variant parent without symlinks: %w", err)
			}
			fd = next
		}
	}
	return fd, filepath.Base(dest), nil
}

func (a *variantAttempt) promoteFile(root, stage, dest string) error {
	fd, name, err := openVariantDestination(root, dest)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	staged, stagedName, err := a.stageParent(stage, false)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(staged) }()
	if err := unix.Linkat(staged, stagedName, fd, name, 0); err != nil {
		return fmt.Errorf("no-replace variant file promotion %s: %w", dest, err)
	}
	return nil
}

func (a *variantAttempt) promoteAudio(root, dest string) error {
	fd, name, err := openVariantDestination(root, dest)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := renameVariantDirNoReplace(a.attemptFD, "audio", fd, name); err != nil {
		return fmt.Errorf("no-replace variant audio promotion %s: %w", dest, err)
	}
	return nil
}
