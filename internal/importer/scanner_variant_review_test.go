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
	rejected, err := f.rejections.IsRejected(f.ctx, f.book.ID, source)
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
				rejected, err := f.rejections.IsRejected(f.ctx, f.book.ID, folder)
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
