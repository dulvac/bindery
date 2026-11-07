//go:build !windows

package importer

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

type legacyQbitEbookFixture struct {
	scanner   *Scanner
	books     *db.BookRepo
	downloads *db.DownloadRepo
	book      *models.Book
	download  *models.Download
	ctx       context.Context
	source    string
	dest      string
	original  []byte
	seed      os.FileInfo
	cleanups  int
}

func newLegacyQbitEbookFixture(t *testing.T) *legacyQbitEbookFixture {
	t.Helper()
	library := t.TempDir()
	s, book, downloads, books, settings, database, ctx := sharedFormatFixture(t, library)
	if err := settings.Set(ctx, "import.mode", "move"); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "We Who Wrestle with God.epub")
	original := bookSized("legacy qbit ebook seed")
	if err := os.WriteFile(source, original, 0o644); err != nil {
		t.Fatal(err)
	}
	seed, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	author, err := db.NewAuthorRepo(database).GetByID(ctx, book.AuthorID)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := s.destRenamer(ctx).DestPath(library, author, book, "", "", source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		t.Fatal(err)
	}
	client := &models.DownloadClient{Type: "qbittorrent", RemoveOnImport: true}
	if err := s.clients.Create(ctx, client); err != nil {
		t.Fatal(err)
	}
	dl := &models.Download{
		GUID: "r15-legacy-qbit-ebook", Title: book.Title, BookID: &book.ID,
		DownloadClientID: &client.ID, Status: models.StateCompleted, Protocol: "torrent",
	}
	if err := downloads.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if dl.VariantID != nil {
		t.Fatal("precondition: download must be legacy/unbound")
	}
	return &legacyQbitEbookFixture{
		scanner: s, books: books, downloads: downloads, book: book, download: dl,
		ctx: ctx, source: source, dest: dest, original: original, seed: seed,
	}
}

func (f *legacyQbitEbookFixture) importEbook() {
	f.scanner.tryImportInternal(f.ctx, f.download, f.source, "qbittorrent", "test-hash", "", func() error {
		f.cleanups++
		return nil
	}, []string{f.source})
}

func (f *legacyQbitEbookFixture) assertSeedAndStage(t *testing.T) {
	t.Helper()
	seed, err := os.Stat(f.source)
	if err != nil || !os.SameFile(f.seed, seed) {
		t.Fatalf("seed inode changed: %v", err)
	}
	if contents, err := os.ReadFile(f.source); err != nil || !bytes.Equal(contents, f.original) {
		t.Fatalf("seed bytes changed: %v", err)
	}
	if f.cleanups != 0 {
		t.Fatalf("qBittorrent cleanup called %d time(s)", f.cleanups)
	}
	entries, err := os.ReadDir(filepath.Dir(f.dest))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".bindery-stage-") {
			t.Errorf("private staged link left behind: %s", entry.Name())
		}
	}
}

func (f *legacyQbitEbookFixture) assertBlockedWithoutBookFile(t *testing.T, reason string) {
	t.Helper()
	got, err := f.downloads.GetByID(f.ctx, f.download.ID)
	if err != nil || got.Status != models.StateImportBlocked || !strings.Contains(got.ErrorMessage, reason) {
		t.Fatalf("collision must block import (%s): %+v, %v", reason, got, err)
	}
	files, err := f.books.ListFiles(f.ctx, f.book.ID)
	if err != nil || len(files) != 0 {
		t.Fatalf("blocked download claimed a book_file: %+v, %v", files, err)
	}
	book, err := f.books.GetByID(f.ctx, f.book.ID)
	if err != nil || book.Status != models.BookStatusWanted {
		t.Fatalf("blocked download marked the book imported: %+v, %v", book, err)
	}
	f.assertSeedAndStage(t)
}

// A preexisting public name is not necessarily in book_files. In particular,
// an untracked EPUB or symlink must survive the final promotion unchanged.
func TestLegacyQbitEbook_UntrackedPublicCollisionNeverReplacesEntry(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			f := newLegacyQbitEbookFixture(t)
			sentinel := filepath.Join(t.TempDir(), "sentinel.epub")
			if err := os.WriteFile(sentinel, []byte("other owner's bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file":
				if err := os.WriteFile(f.dest, []byte("untracked public EPUB"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(sentinel, f.dest); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(f.dest, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.dest, "keep"), []byte("directory contents"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(f.dest)
			if err != nil {
				t.Fatal(err)
			}

			f.importEbook()

			f.assertBlockedWithoutBookFile(t, "file exists")
			after, err := os.Lstat(f.dest)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("public %s inode replaced: %v", kind, err)
			}
			switch kind {
			case "file":
				if got, err := os.ReadFile(f.dest); err != nil || string(got) != "untracked public EPUB" {
					t.Fatalf("public EPUB overwritten: %q, %v", got, err)
				}
			case "symlink":
				if target, err := os.Readlink(f.dest); err != nil || target != sentinel {
					t.Fatalf("symlink replaced: %q, %v", target, err)
				}
			case "directory":
				if got, err := os.ReadFile(filepath.Join(f.dest, "keep")); err != nil || string(got) != "directory contents" {
					t.Fatalf("directory contents changed: %q, %v", got, err)
				}
			}
			if got, err := os.ReadFile(sentinel); err != nil || string(got) != "other owner's bytes" {
				t.Fatalf("symlink target or neighbor changed: %q, %v", got, err)
			}
		})
	}
}

// Inject another writer precisely at the final link syscall, after staging
// and the book_files write. A prior os.Lstat would miss this collision.
func TestLegacyQbitEbook_PublicNameCreatedAtCommitNeverReplaced(t *testing.T) {
	f := newLegacyQbitEbookFixture(t)
	originalLink := osLink
	promotions := 0
	osLink = func(src, dst string) error {
		if dst == f.dest {
			promotions++
			if err := os.WriteFile(dst, []byte("concurrent public EPUB"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return originalLink(src, dst)
	}
	t.Cleanup(func() { osLink = originalLink })

	f.importEbook()

	if promotions != 1 {
		t.Fatalf("final no-replace link not attempted: %d", promotions)
	}
	f.assertBlockedWithoutBookFile(t, "file exists")
	if got, err := os.ReadFile(f.dest); err != nil || string(got) != "concurrent public EPUB" {
		t.Fatalf("concurrent public EPUB replaced: %q, %v", got, err)
	}
}

func TestLegacyQbitEbook_AbsentDestinationPublishesOneHardlink(t *testing.T) {
	f := newLegacyQbitEbookFixture(t)
	f.importEbook()

	got, err := f.downloads.GetByID(f.ctx, f.download.ID)
	if err != nil || got.Status != models.StateImported {
		t.Fatalf("download not imported: %+v, %v", got, err)
	}
	files, err := f.books.ListFiles(f.ctx, f.book.ID)
	if err != nil || len(files) != 1 || files[0].Path != f.dest {
		t.Fatalf("expected one tracked destination: %+v, %v", files, err)
	}
	public, err := os.Stat(f.dest)
	if err != nil || !os.SameFile(f.seed, public) {
		t.Fatalf("public EPUB is not the seeded inode: %v", err)
	}
	if links := public.Sys().(*syscall.Stat_t).Nlink; links != 2 {
		t.Fatalf("expected exactly seed + public link, got %d links", links)
	}
	f.assertSeedAndStage(t)
}

func TestLegacyQbitEbook_EXDEVAtFinalPromotionBlocksWithoutCopy(t *testing.T) {
	f := newLegacyQbitEbookFixture(t)
	originalLink := osLink
	promotions := 0
	osLink = func(src, dst string) error {
		if dst == f.dest {
			promotions++
			return &os.LinkError{Op: "link", Old: src, New: dst, Err: syscall.EXDEV}
		}
		return originalLink(src, dst)
	}
	t.Cleanup(func() { osLink = originalLink })

	f.importEbook()

	if promotions != 1 {
		t.Fatalf("final hardlink not attempted: %d", promotions)
	}
	f.assertBlockedWithoutBookFile(t, "cross-device")
	if _, err := os.Lstat(f.dest); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("EXDEV must not create a public copy: %v", err)
	}
	seed, err := os.Stat(f.source)
	if err != nil {
		t.Fatal(err)
	}
	if links := seed.Sys().(*syscall.Stat_t).Nlink; links != 1 {
		t.Fatalf("EXDEV left a hardlink behind: %d", links)
	}
}

// Other legacy download clients still use the existing replacing copy
// promotion. Only the strict qBittorrent path changes its final syscall.
func TestLegacyNonQbitStagedCopy_RetainsExistingPromotion(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "seed.epub")
	dest := filepath.Join(root, "library", "book.epub")
	if err := os.WriteFile(source, []byte("new import"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("old untracked import"), 0o600); err != nil {
		t.Fatal(err)
	}

	staged, commit, rollback, err := StagedImport(context.Background(), "copy", source, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback()
	if err := commit(); err != nil {
		t.Fatalf("non-strict copy promotion changed: %v", err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "new import" {
		t.Fatalf("non-strict copy did not retain replacement behavior: %q, %v", got, err)
	}
	if _, err := os.Lstat(staged); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("non-strict staging path remains: %v", err)
	}
	if got, err := os.ReadFile(source); err != nil || string(got) != "new import" {
		t.Fatalf("non-strict copy changed source: %q, %v", got, err)
	}
}
