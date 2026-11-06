package importer

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

type variantReviewFixture struct {
	ctx              context.Context
	database         *sql.DB
	scanner          *Scanner
	books            *db.BookRepo
	downloads        *db.DownloadRepo
	history          *db.HistoryRepo
	rejections       *db.VariantRejectionRepo
	book             *models.Book
	variant          models.BookVariant
	library, sources string
}

func newVariantReviewFixture(t *testing.T, title, authorName, format, language string) *variantReviewFixture {
	t.Helper()
	database, err := db.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	books, authors, downloads := db.NewBookRepo(database), db.NewAuthorRepo(database), db.NewDownloadRepo(database)
	author := &models.Author{ForeignID: "variant-review-author", Name: authorName, SortName: authorName, Monitored: true, MetadataProvider: "openlibrary"}
	if err := authors.Create(ctx, author); err != nil {
		t.Fatal(err)
	}
	book := &models.Book{ForeignID: "variant-review-book", AuthorID: author.ID, Title: title, SortTitle: title, MediaType: format, Status: models.BookStatusWanted, Monitored: true, AnyEditionOK: true, MetadataProvider: "openlibrary"}
	if err := books.Create(ctx, book); err != nil {
		t.Fatal(err)
	}
	variants, err := books.VariantRepo().EnsureWanted(ctx, book.ID, format, []string{language})
	if err != nil {
		t.Fatal(err)
	}
	library, sources := t.TempDir(), t.TempDir()
	history := db.NewHistoryRepo(database)
	return &variantReviewFixture{ctx: ctx, database: database, scanner: NewScanner(downloads, db.NewDownloadClientRepo(database), books, authors, history, library, library, "", "", ""),
		books: books, downloads: downloads, history: history, rejections: db.NewVariantRejectionRepo(database), book: book, variant: variants[0], library: library, sources: sources}
}

func (f *variantReviewFixture) addDownload(t *testing.T, title string) *models.Download {
	t.Helper()
	dl := &models.Download{GUID: "variant-test-" + title, Title: title, BookID: &f.book.ID, VariantID: &f.variant.ID, Status: models.StateCompleted, Protocol: "torrent"}
	if err := f.downloads.Create(f.ctx, dl); err != nil {
		t.Fatal(err)
	}
	return dl
}

func (f *variantReviewFixture) importSource(t *testing.T, dl *models.Download, source string) {
	t.Helper()
	f.scanner.tryImportInternal(f.ctx, dl, source, "qbittorrent", "test-hash", "", nil, []string{source})
}

func (f *variantReviewFixture) assertLibraryFiles(t *testing.T, count int) {
	t.Helper()
	files, err := f.books.ListFiles(f.ctx, f.book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != count {
		t.Fatalf("book_files = %+v; want %d", files, count)
	}
	var visible int
	if err := filepath.Walk(f.library, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && IsBookFile(path) {
			visible++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if visible != count {
		t.Fatalf("visible library book files = %d, want %d", visible, count)
	}
}

// The original small language fixtures are below the importer's 4 KiB floor.
// Add an unreferenced, stored ZIP entry instead of changing their sampled text.
func variantEPUBSource(t *testing.T, dir, name, original string) string {
	t.Helper()
	in, err := zip.OpenReader(filepath.Join("testdata", original))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	path := filepath.Join(dir, name)
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	for _, entry := range in.File {
		r, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		w, err := zw.Create(entry.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(w, r); err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
	}
	padding := make([]byte, 8192)
	_, _ = rand.New(rand.NewSource(42)).Read(padding)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "fixture-padding.dat", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(padding); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f *variantReviewFixture) assertReview(t *testing.T, source string) {
	t.Helper()
	f.assertLibraryFiles(t, 0)
	rows, err := f.books.VariantRepo().ListByBook(f.ctx, f.book.ID)
	if err != nil || len(rows) == 0 || rows[0].Status != models.VariantReview {
		t.Fatalf("variant after rejection = %+v, %v; want review", rows, err)
	}
	rejected, err := f.rejections.IsRejected(f.ctx, f.book.ID, f.variant.ID, source)
	if err != nil || !rejected {
		t.Fatalf("rejection for %s = %t, %v", source, rejected, err)
	}
}

func TestVariantImport_EbookEvidenceBeforeAnyLink(t *testing.T) {
	for _, tc := range []struct {
		name, fixture string
		rejected      bool
	}{
		{"Romanian text with false English OPF", "romanian-en-opf.epub", true},
		{"verified English text", "english.epub", false},
		{"mixed text stays unverified", "mixed.epub", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
			source := variantEPUBSource(t, f.sources, "English catalogue label.epub", tc.fixture)
			original, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
			f.importSource(t, dl, source)
			if tc.rejected {
				f.assertReview(t, source)
			} else {
				f.assertLibraryFiles(t, 1)
				files, err := f.books.ListFiles(f.ctx, f.book.ID)
				if err != nil {
					t.Fatal(err)
				}
				if tc.fixture == "mixed.epub" {
					updated, err := f.books.GetByID(f.ctx, f.book.ID)
					if err != nil {
						t.Fatal(err)
					}
					if files[0].Verification != "unverified" || files[0].Language != "" || updated.Language != "" {
						t.Fatalf("mixed content claimed English: file=%+v book=%+v", files[0], updated)
					}
				}
			}
			if after, err := os.ReadFile(source); err != nil || string(after) != string(original) {
				t.Fatalf("torrent source modified: %v", err)
			}
		})
	}
}

func TestVariantImport_AudioWorkEvidencePrecedesIdempotency(t *testing.T) {
	for _, tc := range []struct {
		name, release, album, artist string
		preexisting, reject          bool
	}{
		{"biography", "Dreamer of Dune: The Biography of Frank Herbert by Brian Herbert [ENG / M4B]", "Dreamer of Dune: The Biography of Frank Herbert", "Brian Herbert", false, true},
		{"biography with existing Dune audio", "Dreamer of Dune: The Biography of Frank Herbert by Brian Herbert [ENG / M4B]", "Dreamer of Dune: The Biography of Frank Herbert", "Brian Herbert", true, true},
		{"actual novel remains eligible", "Dune by Frank Herbert [ENG / M4B]", "Dune", "Frank Herbert", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
			folder := filepath.Join(f.sources, "Dune audio")
			if err := os.Mkdir(folder, 0o755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(folder, "Dune.m4b")
			if err := os.WriteFile(file, mp4File(atom("\xa9nam", dataAtom(1, []byte("Chapter 1"))), atom("\xa9alb", dataAtom(1, []byte(tc.album))), atom("\xa9ART", dataAtom(1, []byte(tc.artist)))), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.preexisting {
				prior := filepath.Join(f.library, "already-imported")
				if err := os.Mkdir(prior, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := f.books.AddBookFile(f.ctx, f.book.ID, models.MediaTypeAudiobook, prior); err != nil {
					t.Fatal(err)
				}
			}
			dl := f.addDownload(t, tc.release)
			f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "test-hash", "", nil, []string{file})
			if tc.reject {
				rows, err := f.books.ListFiles(f.ctx, f.book.ID)
				if err != nil || len(rows) != map[bool]int{true: 1, false: 0}[tc.preexisting] {
					t.Fatalf("audio attached despite wrong work: %+v, %v", rows, err)
				}
				rejected, err := f.rejections.IsRejected(f.ctx, f.book.ID, f.variant.ID, folder)
				if err != nil || !rejected {
					t.Fatalf("audio folder rejection = %t, %v", rejected, err)
				}
				variants, err := f.books.VariantRepo().ListByBook(f.ctx, f.book.ID)
				if err != nil || variants[0].Status != models.VariantReview {
					t.Fatalf("variant = %+v, %v", variants, err)
				}
			} else {
				f.assertLibraryFiles(t, 1)
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatalf("torrent audio removed: %v", err)
			}
		})
	}
}

func TestVariantImport_OverrideIsAuditedForExactDownloadAndNeverWrongWork(t *testing.T) {
	for _, tc := range []struct {
		name                                                         string
		auditForThis, named, reason, wrongWork, otherVariant, reject bool
	}{
		{"no audit", false, false, false, false, false, true},
		{"other download audit", false, true, true, false, false, true},
		{"other variant audit", true, true, true, false, true, true},
		{"unnamed audit", true, false, true, false, false, true},
		{"empty reason audit", true, true, false, false, false, true},
		{"exact admin audit", true, true, true, false, false, false},
		{"wrong work despite exact audit", true, true, true, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
			source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "romanian-en-opf.epub")
			release := "English catalogue label [ROM / EPUB]"
			if tc.wrongWork {
				release = "Dreamer of Dune by Brian Herbert [ROM / EPUB]"
			}
			dl := f.addDownload(t, release)
			if tc.named || tc.auditForThis {
				id := dl.ID
				if !tc.auditForThis {
					id++
				}
				actor := int64(0)
				if tc.named {
					actor = 42
				}
				variantID := f.variant.ID
				if tc.otherVariant {
					others, err := f.books.VariantRepo().EnsureWanted(f.ctx, f.book.ID, models.MediaTypeEbook, []string{"rum"})
					if err != nil {
						t.Fatal(err)
					}
					variantID = others[0].ID
				}
				why := ""
				if tc.reason {
					why = "owner approved this release"
				}
				data, _ := json.Marshal(map[string]any{"actorUserId": actor, "variantId": variantID, "downloadId": id, "reason": why})
				if err := f.history.Create(f.ctx, &models.HistoryEvent{BookID: &f.book.ID, EventType: "variantLanguageOverride", SourceTitle: release, Data: string(data)}); err != nil {
					t.Fatal(err)
				}
			}
			f.importSource(t, dl, source)
			if tc.reject {
				f.assertReview(t, source)
			} else {
				f.assertLibraryFiles(t, 1)
				files, err := f.books.ListFiles(f.ctx, f.book.ID)
				if err != nil {
					t.Fatal(err)
				}
				if files[0].Language == "eng" || files[0].Verification == "verified" {
					t.Fatalf("override relabeled Romanian content as English: %+v", files[0])
				}
			}
		})
	}
}

// A rejected EPUB inside the torrent's own folder must not reject the folder,
// and an English mismatch must not condemn the Romanian variant of this work.
func TestVariantImport_WrongLanguageRejectsOnlyExactFileAndRequestedVariant(t *testing.T) {
	for _, next := range []string{"Romanian source", "English sibling"} {
		t.Run(next, func(t *testing.T) {
			f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
			folder := filepath.Join(f.sources, "own torrent folder")
			if err := os.Mkdir(folder, 0o755); err != nil {
				t.Fatal(err)
			}
			romanian := variantEPUBSource(t, folder, "English catalogue label (Romanian).epub", "romanian-en-opf.epub")
			english := variantEPUBSource(t, folder, "English catalogue label (English).epub", "english.epub")
			wrong := f.addDownload(t, "English catalogue label [ENG / EPUB]")
			f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), wrong, folder, "qbittorrent", "test-hash", "", nil, []string{romanian})
			f.assertReview(t, romanian)
			paths, err := f.rejections.ListSourcePaths(f.ctx, f.book.ID)
			if err != nil || len(paths) != 1 || paths[0] != romanian {
				t.Fatalf("recorded rejection = %q, %v; want exact EPUB, not folder", paths, err)
			}
			var source string
			var variantID int64
			if next == "Romanian source" {
				other, err := f.books.VariantRepo().EnsureWanted(f.ctx, f.book.ID, models.MediaTypeEbook, []string{"rum"})
				if err != nil || len(other) != 1 {
					t.Fatalf("Romanian variant: %+v %v", other, err)
				}
				variantID = other[0].ID
				source = romanian
			} else {
				variantID = f.variant.ID
				source = english
			}
			// The reviewed English request cannot claim another English grab;
			// retry it with its listed sibling, or make a separate Romanian request.
			nextDownload := wrong
			if next == "Romanian source" {
				nextDownload = &models.Download{GUID: "requested-romanian", Title: "English catalogue label [ROM / EPUB]", BookID: &f.book.ID, VariantID: &variantID, Status: models.StateCompleted, Protocol: "torrent"}
				if err := f.downloads.Create(f.ctx, nextDownload); err != nil {
					t.Fatal(err)
				}
			}
			f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), nextDownload, folder, "qbittorrent", "test-hash", "", nil, []string{source})
			files, err := f.books.ListFiles(f.ctx, f.book.ID)
			if err != nil || len(files) != 1 {
				t.Fatalf("valid variant source did not import: %+v, %v", files, err)
			}
			linked, err := os.Lstat(files[0].Path)
			if err != nil {
				t.Fatal(err)
			}
			assessed, err := os.Lstat(source)
			if err != nil || !os.SameFile(linked, assessed) {
				t.Fatalf("wrong source linked for %s: %v", next, err)
			}
		})
	}
}

// osLink is the narrow placement boundary: replace only the temporary test
// source after evidence was read but just before placement reopens its path.
func replaceVariantSourceOnLink(t *testing.T, source string, replacement []byte) (string, *bool) {
	t.Helper()
	originalLink := osLink
	original := source + ".assessed"
	swapped := false
	osLink = func(src, dst string) error {
		if src == source && !swapped {
			if err := os.Rename(source, original); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(source, replacement, 0o600); err != nil {
				t.Fatal(err)
			}
			swapped = true
		}
		return originalLink(src, dst)
	}
	t.Cleanup(func() { osLink = originalLink })
	return original, &swapped
}

func TestVariantImport_ReplacedEbookAfterEvidenceNeverReachesLibrary(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	bad := variantEPUBSource(t, f.sources, "unchecked.epub", "romanian-en-opf.epub")
	unchecked, err := os.ReadFile(bad)
	if err != nil {
		t.Fatal(err)
	}
	original, swapped := replaceVariantSourceOnLink(t, source, unchecked)
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	f.importSource(t, dl, source)
	if !*swapped {
		t.Fatal("source was not replaced at placement boundary")
	}
	f.assertLibraryFiles(t, 0)
	if _, err := os.Stat(original); err != nil {
		t.Fatalf("assessed source was removed: %v", err)
	}
	got, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || got.Status == models.StateImported {
		t.Fatalf("unchecked EPUB imported: %+v %v", got, err)
	}
}

func TestVariantImport_ReplacedAudioAfterEvidenceNeverReachesLibrary(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
	folder := filepath.Join(f.sources, "own torrent folder")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(folder, "Dune.m4b")
	if err := os.WriteFile(source, mp4File(atom("\xa9alb", dataAtom(1, []byte("Dune"))), atom("\xa9ART", dataAtom(1, []byte("Frank Herbert")))), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := mp4File(atom("\xa9alb", dataAtom(1, []byte("Dreamer of Dune: The Biography of Frank Herbert"))), atom("\xa9ART", dataAtom(1, []byte("Brian Herbert"))))
	original, swapped := replaceVariantSourceOnLink(t, source, bad)
	dl := f.addDownload(t, "Dune by Frank Herbert [ENG / M4B]")
	f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "test-hash", "", nil, []string{source})
	if !*swapped {
		t.Fatal("audio was not replaced at placement boundary")
	}
	f.assertLibraryFiles(t, 0)
	if _, err := os.Stat(original); err != nil {
		t.Fatalf("assessed audio was removed: %v", err)
	}
	got, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || got.Status == models.StateImported {
		t.Fatalf("unchecked audio imported: %+v %v", got, err)
	}
}

// A hardlink shares its inode with the torrent source. A write that changes
// the metadata after assessment must invalidate the newly created link.
func TestVariantImport_ModifiedAudioInodeAfterLinkRollsBackNewLink(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
	folder := filepath.Join(f.sources, "recording")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(folder, "Dune.m4b")
	if err := os.WriteFile(source, mp4File(atom("\xa9alb", dataAtom(1, []byte("Dune"))), atom("\xa9ART", dataAtom(1, []byte("Frank Herbert")))), 0o600); err != nil {
		t.Fatal(err)
	}
	originalLink := osLink
	changed := false
	osLink = func(src, dst string) error {
		err := originalLink(src, dst)
		if err == nil && src == source && !changed {
			later := time.Now().Add(2 * time.Hour)
			if err := os.Chtimes(dst, later, later); err != nil {
				t.Fatal(err)
			}
			changed = true
		}
		return err
	}
	t.Cleanup(func() { osLink = originalLink })
	dl := f.addDownload(t, "Dune by Frank Herbert [ENG / M4B]")
	f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "test-hash", "", nil, []string{source})
	if !changed {
		t.Fatal("file was not changed immediately after link")
	}
	f.assertLibraryFiles(t, 0)
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("original source removed: %v", err)
	}
}

// Copy/move paths cannot prove a final hardlink to the assessed inode. Until
// variant-aware non-torrent placement exists, they must fail closed instead.
func TestVariantImport_NonHardlinkModeDoesNotPlaceUnverifiableVariant(t *testing.T) {
	for _, audio := range []bool{false, true} {
		format := models.MediaTypeEbook
		if audio {
			format = models.MediaTypeAudiobook
		}
		t.Run(format, func(t *testing.T) {
			f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", format, "eng")
			settings := db.NewSettingsRepo(f.database)
			if err := settings.Set(f.ctx, "import.mode", "copy"); err != nil {
				t.Fatal(err)
			}
			f.scanner.WithSettings(settings)
			var source, folder string
			if audio {
				folder = filepath.Join(f.sources, "recording")
				if err := os.Mkdir(folder, 0o755); err != nil {
					t.Fatal(err)
				}
				source = filepath.Join(folder, "English catalogue label.m4b")
				if err := os.WriteFile(source, mp4File(atom("\xa9alb", dataAtom(1, []byte("English catalogue label"))), atom("\xa9ART", dataAtom(1, []byte("Sample Author")))), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				source = variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
				folder = source
			}
			dl := f.addDownload(t, "English catalogue label [ENG / "+map[bool]string{true: "M4B", false: "EPUB"}[audio]+"]")
			dl.Protocol = "usenet"
			if _, err := f.database.ExecContext(f.ctx, `UPDATE downloads SET protocol = 'usenet' WHERE id = ?`, dl.ID); err != nil {
				t.Fatal(err)
			}
			f.scanner.tryImportInternal(f.ctx, dl, folder, "sabnzbd", "job", "", nil, []string{source})
			f.assertLibraryFiles(t, 0)
			if _, err := os.Stat(source); err != nil {
				t.Fatalf("source changed: %v", err)
			}
		})
	}
}

// A replacement of the staged inode after staging but before final promotion
// must not leave the newly created link behind or remove an unrelated entry.
func TestVariantImport_EarlierAudioTrackChangedWhileLinkingLaterTrackIsRolledBack(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
	folder := filepath.Join(f.sources, "recording")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{filepath.Join(folder, "01.m4b"), filepath.Join(folder, "02.m4b")}
	for _, source := range files {
		if err := os.WriteFile(source, mp4File(atom("\xa9alb", dataAtom(1, []byte("Dune"))), atom("\xa9ART", dataAtom(1, []byte("Frank Herbert")))), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	originalLink := osLink
	changed := false
	osLink = func(src, dst string) error {
		err := originalLink(src, dst)
		if err == nil && src == files[1] && !changed {
			later := time.Now().Add(2 * time.Hour)
			if err := os.Chtimes(files[0], later, later); err != nil {
				t.Fatal(err)
			}
			changed = true
		}
		return err
	}
	t.Cleanup(func() { osLink = originalLink })
	dl := f.addDownload(t, "Dune by Frank Herbert [ENG / M4B]")
	f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "test-hash", "", nil, files)
	if !changed {
		t.Fatal("first track was not modified while linking the second")
	}
	f.assertLibraryFiles(t, 0)
	for _, source := range files {
		if _, err := os.Stat(source); err != nil {
			t.Fatalf("torrent source removed: %s: %v", source, err)
		}
	}
}

func TestVariantImport_ReplacedStageBeforeFinalEbookLinkRollsBackNewLink(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	bad := variantEPUBSource(t, f.sources, "unchecked.epub", "romanian-en-opf.epub")
	unchecked, err := os.ReadFile(bad)
	if err != nil {
		t.Fatal(err)
	}
	author, err := db.NewAuthorRepo(f.database).GetByID(f.ctx, f.book.AuthorID)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := f.scanner.destRenamer(f.ctx).DestPath(f.library, author, f.book, "", "", source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(filepath.Dir(dest), "keep.sentinel")
	if err := os.WriteFile(unrelated, []byte("not this import"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalLink := osLink
	swapped := false
	osLink = func(src, dst string) error {
		if !swapped && src != source && filepath.Dir(src) == filepath.Dir(dst) {
			if err := os.Remove(src); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(src, unchecked, 0o600); err != nil {
				t.Fatal(err)
			}
			swapped = true
		}
		return originalLink(src, dst)
	}
	t.Cleanup(func() { osLink = originalLink })
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	f.importSource(t, dl, source)
	if !swapped {
		t.Fatal("stage was not replaced at final link boundary")
	}
	f.assertLibraryFiles(t, 0)
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("new unchecked library link remains: %v", err)
	}
	if content, err := os.ReadFile(unrelated); err != nil || string(content) != "not this import" {
		t.Fatalf("unrelated library file removed or changed: %q %v", content, err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("assessed source removed: %v", err)
	}
}

func TestVariantImport_WrongWorkIsReassessedForEachVariant(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "biography.epub", "english.epub")
	first := f.addDownload(t, "Dreamer of Dune by Brian Herbert [ENG / EPUB]")
	f.importSource(t, first, source)
	f.assertReview(t, source)
	romanian, err := f.books.VariantRepo().EnsureWanted(f.ctx, f.book.ID, models.MediaTypeEbook, []string{"rum"})
	if err != nil || len(romanian) != 1 {
		t.Fatalf("Romanian variant: %+v %v", romanian, err)
	}
	second := &models.Download{GUID: "wrong-work-other-variant", Title: "Dreamer of Dune by Brian Herbert [ROM / EPUB]", BookID: &f.book.ID, VariantID: &romanian[0].ID, Status: models.StateCompleted, Protocol: "torrent"}
	if err := f.downloads.Create(f.ctx, second); err != nil {
		t.Fatal(err)
	}
	f.importSource(t, second, source)
	f.assertLibraryFiles(t, 0)
	if rejected, err := f.rejections.IsRejected(f.ctx, f.book.ID, romanian[0].ID, source); err != nil || !rejected {
		t.Fatalf("second variant did not independently reject wrong work: %t %v", rejected, err)
	}
	paths, err := f.rejections.ListSourcePaths(f.ctx, f.book.ID)
	if err != nil || len(paths) != 2 {
		t.Fatalf("wrong work was not reassessed for both variants: %q %v", paths, err)
	}
}

func TestVariantImport_MixedLanguageBundleOnlyLinksRequestedFile(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	english := variantEPUBSource(t, f.sources, "English catalogue label (English).epub", "english.epub")
	romanian := variantEPUBSource(t, f.sources, "English catalogue label.epub", "romanian-en-opf.epub")
	unknown := variantEPUBSource(t, f.sources, "English catalogue label (uncertain).epub", "mixed.epub")
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	f.scanner.tryImportInternal(f.ctx, dl, f.sources, "qbittorrent", "test-hash", "", nil, []string{romanian, unknown, english})
	files, err := f.books.ListFiles(f.ctx, f.book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("bundle attached %+v; want English file only", files)
	}
	if !strings.HasSuffix(files[0].Path, ".epub") {
		t.Fatalf("unexpected attached file: %+v", files[0])
	}
	linked, err := os.Stat(files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	wanted, err := os.Stat(english)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(linked, wanted) {
		t.Fatalf("bundle attached non-English or uncertain source: %+v", files[0])
	}
	for _, other := range []string{romanian, unknown} {
		src, err := os.Stat(other)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(src, linked) {
			t.Fatalf("wrong-language source attached: %s", other)
		}
	}
}
