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
	"syscall"
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
	library, sources := filepath.Join(t.TempDir(), "books"), t.TempDir()
	if err := os.Mkdir(library, 0o700); err != nil {
		t.Fatal(err)
	}
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

func (f *variantReviewFixture) assertVariantPlaced(t *testing.T, dl *models.Download) {
	t.Helper()
	variantImportedDownload(t, f, dl.ID)
	rows := variantImportedFiles(t, f, 1)
	if rows[0].VariantID == nil || *rows[0].VariantID != *dl.VariantID {
		t.Fatalf("file not associated with selected variant: %+v", rows)
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
				f.assertVariantPlaced(t, dl)
				rows := variantImportedFiles(t, f, 1)
				if tc.fixture == "mixed.epub" && (rows[0].Verification != "unverified" || rows[0].Language != "") {
					t.Fatalf("uncertain language was relabeled: %+v", rows[0])
				}
			}
			if after, err := os.ReadFile(source); err != nil || string(after) != string(original) {
				t.Fatalf("torrent source modified: %v", err)
			}
		})
	}
}

// A manual or older grab can bypass search relevance. Its completed torrent
// must still pass release identity review before any public hardlink is made.
func TestVariantImport_ExactTitleWrongAudioAuthorNeverLinks(t *testing.T) {
	for _, tc := range []struct {
		name, release, album, artist string
		unreadable, override, reject bool
	}{
		{"wrong author, no tags", "Dune by Brian Herbert [ENG / M4B]", "", "", false, false, true},
		{"wrong author, unreadable tags", "Dune by Brian Herbert [ENG / M4B]", "", "", true, false, true},
		{"wrong author role before narrator", "Dune - Brian Herbert - author, Simon Vance - narrator [ENG/M4B]", "", "", false, false, true},
		{"by author role before narrator", "Dune by Brian Herbert - author, Simon Vance - narrator [ENG/M4B]", "", "", false, false, true},
		{"wrong author role after narrator", "Dune - Simon Vance - narrator, Brian Herbert - author [ENG/M4B]", "", "", false, false, true},
		{"wrong author role with matching narrator tag", "Dune - Brian Herbert - author, Frank Herbert - narrator [ENG/M4B]", "Dune", "Frank Herbert", false, false, true},
		{"wrong author, album Dune and bare narrator Brian", "Dune by Brian Herbert [ENG / M4B]", "Dune", "Brian Herbert", false, false, true},
		{"wrong author, album Dune and read-by narrator", "Dune by Brian Herbert [ENG / M4B]", "Dune", "Read by Brian Herbert", false, false, true},
		{"wrong author, named admin language override", "Dune by Brian Herbert [ROM / M4B]", "Dune", "Frank Herbert", false, true, true},
		{"right author, no tags", "Dune by Frank Herbert [ENG / M4B]", "", "", false, false, false},
		{"right author role before narrator", "Dune - Frank Herbert - author, Brian Herbert - narrator [ENG/M4B]", "", "", false, false, false},
		{"right author role alone", "Dune - Frank Herbert - author [ENG/M4B]", "", "", false, false, false},
		{"author-first release", "Frank Herbert - Dune [ENG / M4B]", "Dune", "Frank Herbert", false, false, false},
		{"right author, narrator Brian", "Dune by Frank Herbert [ENG / M4B]", "Dune", "Brian Herbert", false, false, false},
		{"narrator credit is not an author", "Dune - Read by Brian Herbert [ENG / M4B]", "Dune", "", false, false, false},
		{"by narrator role is not an author", "Dune by Brian Herbert - narrator [ENG/M4B]", "", "", false, false, false},
		{"dash narrator role is not an author", "Dune - Brian Herbert - narrator [ENG/M4B]", "", "", false, false, false},
		{"same author retitle", "Duna by Frank Herbert [ENG / M4B]", "Dune", "Frank Herbert", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
			folder := filepath.Join(f.sources, "recording")
			if err := os.Mkdir(folder, 0o755); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(folder, "Dune.m4b")
			data := mp4File()
			if tc.album != "" || tc.artist != "" {
				data = mp4File(atom("\xa9alb", dataAtom(1, []byte(tc.album))), atom("\xa9ART", dataAtom(1, []byte(tc.artist))))
			}
			if tc.unreadable {
				data = []byte("not an MP4")
			}
			if err := os.WriteFile(source, data, 0o644); err != nil {
				t.Fatal(err)
			}
			dl := f.addDownload(t, tc.release)
			if tc.override {
				audit, _ := json.Marshal(map[string]any{"actorUserId": 42, "variantId": f.variant.ID,
					"downloadId": dl.ID, "reason": "admin approved this language"})
				if err := f.history.Create(f.ctx, &models.HistoryEvent{BookID: &f.book.ID,
					EventType: "variantLanguageOverride", SourceTitle: tc.release, Data: string(audit)}); err != nil {
					t.Fatal(err)
				}
			}
			f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder,
				"qbittorrent", "test-hash", "", nil, []string{source})
			if tc.reject {
				f.assertReview(t, folder)
				if got, err := f.downloads.GetByID(f.ctx, dl.ID); err != nil || got.Status == models.StateImported {
					t.Fatalf("wrong work imported: %+v, %v", got, err)
				}
			} else {
				f.assertVariantPlaced(t, dl)
			}
			if after, err := os.ReadFile(source); err != nil || string(after) != string(data) {
				t.Fatalf("torrent source changed: %v", err)
			}
		})
	}
}

func TestVariantImport_ExactTitleKnownAuthorAliasStaysEligible(t *testing.T) {
	for _, release := range []string{
		"Dune by Francis Albus [ENG / M4B]",
		"Dune - Francis Albus - author, Simon Vance - narrator [ENG/M4B]",
	} {
		t.Run(release, func(t *testing.T) {
			f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
			aliases := db.NewAuthorAliasRepo(f.database)
			if err := aliases.Create(f.ctx, &models.AuthorAlias{AuthorID: f.book.AuthorID, Name: "Francis Albus"}); err != nil {
				t.Fatal(err)
			}
			folder := filepath.Join(f.sources, "recording")
			if err := os.Mkdir(folder, 0o755); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(folder, "Dune.m4b")
			if err := os.WriteFile(source, mp4File(atom("\xa9alb", dataAtom(1, []byte("Dune")))), 0o644); err != nil {
				t.Fatal(err)
			}
			dl := f.addDownload(t, release)
			author, err := db.NewAuthorRepo(f.database).GetByID(f.ctx, f.book.AuthorID)
			if err != nil {
				t.Fatal(err)
			}
			without, err := f.scanner.AssessVariantEvidence(f.ctx, dl, f.book, author, &f.variant, []string{source}, false)
			if err != nil || !without.WrongWork {
				t.Fatalf("unrecognized author credit must require review: %+v, %v", without, err)
			}
			f.scanner.WithAuthorAliases(aliases)
			f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder,
				"qbittorrent", "test-hash", "", nil, []string{source})
			f.assertVariantPlaced(t, dl)
			if _, err := os.Stat(source); err != nil {
				t.Fatalf("seed was changed: %v", err)
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
		{"bracketed author narrator novel remains eligible", "Dune [Narrated by Frank Herbert] [ENG / M4B]", "Dune", "Frank Herbert", false, false},
		{"bracketed performed by author novel remains eligible", "Dune [Performed by Frank Herbert] [ENG / M4B]", "Dune", "Frank Herbert", false, false},
		{"sequel performed by author with separate author credit never links", "Dune Messiah [Performed by Frank Herbert] by Frank Herbert [ENG / M4B]", "", "", false, true},
		{"same author sequel release and tags never link", "Dune Messiah by Frank Herbert [ENG / M4B]", "Dune Messiah", "Frank Herbert", false, true},
		{"narrator cannot hide same author sequel without tags", "Dune Messiah by Frank Herbert - Narrated by Simon Vance [ENG / M4B]", "", "", false, true},
		{"dotted byline edition cannot hide sequel without tags", "Dune.Messiah.[Unabridged].by.Frank.Herbert.[ENG / M4B]", "", "", false, true},
		{"exact release cannot hide same author sequel album", "Dune by Frank Herbert [ENG / M4B]", "Dune Messiah", "Frank Herbert", false, true},
		{"colon sequel album never links", "Dune by Frank Herbert [ENG / M4B]", "Dune: Messiah", "Frank Herbert", false, true},
		{"sequel with novel suffix never links", "Dune Messiah: A Novel by Frank Herbert [ENG / M4B]", "Dune Messiah: A Novel", "Frank Herbert", false, true},
		{"wrong bracketed sequel album never links", "Dune by Frank Herbert [ENG / M4B]", "Dune Messiah [Unabridged]", "Frank Herbert", false, true},
		{"wrong parenthesized sequel album never links", "Dune by Frank Herbert [ENG / M4B]", "Dune Messiah (A Novel)", "Frank Herbert", false, true},
		{"illustrated novel album may link", "Dune by Frank Herbert [ENG / M4B]", "Dune (Illustrated)", "Frank Herbert", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
			folder := filepath.Join(f.sources, "Dune audio")
			if err := os.Mkdir(folder, 0o755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(folder, "Dune.m4b")
			if err := os.WriteFile(file, mp4File(atom("\xa9nam", dataAtom(1, []byte("Chapter 1"))), atom("\xa9alb", dataAtom(1, []byte(tc.album))), atom("\xa9ART", dataAtom(1, []byte(tc.artist)))), 0o644); err != nil {
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
				f.assertVariantPlaced(t, dl)
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
				f.assertVariantPlaced(t, dl)
				rows := variantImportedFiles(t, f, 1)
				if rows[0].Verification != "unverified" || rows[0].Language != "rum" {
					t.Fatalf("override must retain detected Romanian without verifying English: %+v", rows[0])
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
			if next == "Romanian source" {
				f.assertVariantPlaced(t, nextDownload)
			} else {
				// The blocked English download cannot re-enter via a generic
				// import. Keep its original rejection reason until a named retry.
				variantBlocked(t, f, nextDownload, "ebook text language rum differs from requested eng")
				f.assertLibraryFiles(t, 0)
			}
			if _, err := os.Lstat(source); err != nil {
				t.Fatalf("eligible source disappeared for %s: %v", next, err)
			}
		})
	}
}

// Replace the source at the hardlink boundary. Staging must detect that the
// assessed inode disappeared and must not promote the replacement.
func replaceVariantSourceOnLink(t *testing.T, source string, replacement []byte) (string, *bool) {
	t.Helper()
	originalLink := variantStageLink
	original := source + ".assessed"
	swapped := false
	variantStageLink = func(src string, parentFD int, name string) error {
		if src == source && !swapped {
			if err := os.Rename(source, original); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(source, replacement, 0o600); err != nil {
				t.Fatal(err)
			}
			swapped = true
		}
		return originalLink(src, parentFD, name)
	}
	t.Cleanup(func() { variantStageLink = originalLink })
	return original, &swapped
}

func TestVariantImport_EbookEvidenceNeverStartsPublicLink(t *testing.T) {
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
		t.Fatal("test did not replace source during private staging")
	}
	variantBlocked(t, f, dl, "variant source")
	f.assertLibraryFiles(t, 0)
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("replacement removed: %v", err)
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatalf("assessed seed removed: %v", err)
	}
	got, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || got.Status == models.StateImported {
		t.Fatalf("unchecked EPUB imported: %+v %v", got, err)
	}
}

func TestVariantImport_AudioEvidenceNeverStartsPublicLink(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
	folder := filepath.Join(f.sources, "own torrent folder")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(folder, "Dune.m4b")
	if err := os.WriteFile(source, mp4File(atom("\xa9alb", dataAtom(1, []byte("Dune"))), atom("\xa9ART", dataAtom(1, []byte("Frank Herbert")))), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := mp4File(atom("\xa9alb", dataAtom(1, []byte("Dreamer of Dune: The Biography of Frank Herbert"))), atom("\xa9ART", dataAtom(1, []byte("Brian Herbert"))))
	original, swapped := replaceVariantSourceOnLink(t, source, bad)
	dl := f.addDownload(t, "Dune by Frank Herbert [ENG / M4B]")
	f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "test-hash", "", nil, []string{source})
	if !*swapped {
		t.Fatal("test did not replace audio source during private staging")
	}
	variantBlocked(t, f, dl, "variant source")
	f.assertLibraryFiles(t, 0)
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("replacement removed: %v", err)
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatalf("assessed audio removed: %v", err)
	}
	got, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || got.Status == models.StateImported {
		t.Fatalf("unchecked audio imported: %+v %v", got, err)
	}
}

// Modifying a staged hardlink changes the assessed source inode's mtime.
func TestVariantImport_AudioStageMutationCannotPromote(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
	folder := filepath.Join(f.sources, "recording")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(folder, "Dune.m4b")
	if err := os.WriteFile(source, mp4File(atom("\xa9alb", dataAtom(1, []byte("Dune"))), atom("\xa9ART", dataAtom(1, []byte("Frank Herbert")))), 0o644); err != nil {
		t.Fatal(err)
	}
	originalLink := variantStageLink
	changed := false
	variantStageLink = func(src string, parentFD int, name string) error {
		err := originalLink(src, parentFD, name)
		if err == nil && src == source && !changed {
			later := time.Now().Add(2 * time.Hour)
			if err := os.Chtimes(source, later, later); err != nil {
				t.Fatal(err)
			}
			changed = true
		}
		return err
	}
	t.Cleanup(func() { variantStageLink = originalLink })
	dl := f.addDownload(t, "Dune by Frank Herbert [ENG / M4B]")
	f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "test-hash", "", nil, []string{source})
	if !changed {
		t.Fatal("stage hook was not exercised")
	}
	variantBlocked(t, f, dl, "variant source")
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

// A changed first track after staging the second stops the whole folder.
func TestVariantImport_MultiTrackAudioStageMutationCannotPromote(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
	folder := filepath.Join(f.sources, "recording")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{filepath.Join(folder, "01.m4b"), filepath.Join(folder, "02.m4b")}
	for _, source := range files {
		if err := os.WriteFile(source, mp4File(atom("\xa9alb", dataAtom(1, []byte("Dune"))), atom("\xa9ART", dataAtom(1, []byte("Frank Herbert")))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	originalLink := variantStageLink
	changed := false
	variantStageLink = func(src string, parentFD int, name string) error {
		err := originalLink(src, parentFD, name)
		if err == nil && src == files[1] && !changed {
			later := time.Now().Add(2 * time.Hour)
			if err := os.Chtimes(files[0], later, later); err != nil {
				t.Fatal(err)
			}
			changed = true
		}
		return err
	}
	t.Cleanup(func() { variantStageLink = originalLink })
	dl := f.addDownload(t, "Dune by Frank Herbert [ENG / M4B]")
	f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "test-hash", "", nil, files)
	if !changed {
		t.Fatal("stage hook was not exercised")
	}
	variantBlocked(t, f, dl, "variant source")
	f.assertLibraryFiles(t, 0)
	for _, source := range files {
		if _, err := os.Stat(source); err != nil {
			t.Fatalf("torrent source removed: %s: %v", source, err)
		}
	}
}

func TestVariantImport_EbookDoesNotCreatePublicStagingPath(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
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
	originalLink := variantStageLink
	stagedPrivately := false
	variantStageLink = func(src string, parentFD int, name string) error {
		if src != source {
			t.Fatalf("stage linked unreviewed source %s", src)
		}
		dup, err := syscall.Dup(parentFD)
		if err != nil {
			t.Fatal(err)
		}
		stagedParent := os.NewFile(uintptr(dup), "stage parent")
		privateInfo, statErr := stagedParent.Stat()
		_ = stagedParent.Close()
		publicInfo, publicErr := os.Stat(filepath.Dir(dest))
		if statErr != nil || publicErr != nil || os.SameFile(privateInfo, publicInfo) {
			t.Fatalf("variant staged in the public directory: %v %v", statErr, publicErr)
		}
		stagedPrivately = true
		return originalLink(src, parentFD, name)
	}
	t.Cleanup(func() { variantStageLink = originalLink })
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	f.importSource(t, dl, source)
	if !stagedPrivately {
		t.Fatal("private stage link not exercised")
	}
	f.assertVariantPlaced(t, dl)
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("legacy unsuffixed destination was created: %v", err)
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

// A rejected EPUB is an exact-file verdict, not a verdict on its torrent or
// its English sibling. A reviewed variant still needs an explicit admin
// transition before that valid sibling may be placed.
func TestVariantImport_RejectedSiblingStillNeedsAdminTransition(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	romanian := variantEPUBSource(t, f.sources, "English catalogue label (Romanian).epub", "romanian-en-opf.epub")
	english := variantEPUBSource(t, f.sources, "English catalogue label (English).epub", "english.epub")
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	if err := f.rejections.Record(f.ctx, f.book.ID, f.variant.ID, dl.ID, romanian, "Romanian is not this English variant"); err != nil {
		t.Fatal(err)
	}
	if err := f.books.VariantRepo().SetStatus(f.ctx, f.variant.ID, models.VariantReview); err != nil {
		t.Fatal(err)
	}
	author, err := db.NewAuthorRepo(f.database).GetByID(f.ctx, f.book.AuthorID)
	if err != nil {
		t.Fatal(err)
	}
	selected, reviewed, reason, err := f.scanner.reviewVariantImport(f.ctx, dl, f.book, author, models.MediaTypeEbook, f.sources, []string{romanian, english})
	if err != nil || reviewed || len(selected) != 1 || selected[0].path != english {
		t.Fatalf("eligible English sibling lost: selected=%+v reviewed=%t reason=%q err=%v", selected, reviewed, reason, err)
	}
	f.scanner.tryImportInternal(f.ctx, dl, f.sources, "qbittorrent", "test-hash", "", nil, []string{romanian, english})
	variantBlocked(t, f, dl, "in review")
	f.assertLibraryFiles(t, 0)
	if rejected, err := f.rejections.IsRejected(f.ctx, f.book.ID, f.variant.ID, english); err != nil || rejected {
		t.Fatalf("eligible sibling rejected: %t %v", rejected, err)
	}
	for _, source := range []string{romanian, english} {
		if _, err := os.Stat(source); err != nil {
			t.Fatalf("torrent source changed: %s: %v", source, err)
		}
	}
}

// A second writer can replace a promoted folder before DB tracking. Never
// claim or unlink the replacement, even when the import fails.
func TestVariantImport_PublicAudioReplacementSurvivesPlacementRace(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
	folder := filepath.Join(f.sources, "Dune recording")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(folder, "Dune.m4b")
	if err := os.WriteFile(source, mp4File(atom("\xa9alb", dataAtom(1, []byte("Dune"))), atom("\xa9ART", dataAtom(1, []byte("Frank Herbert")))), 0o644); err != nil {
		t.Fatal(err)
	}
	held := filepath.Join(f.library, "another-writer")
	if err := os.Mkdir(held, 0o700); err != nil {
		t.Fatal(err)
	}
	const sentinel = "another writer's irreplaceable file"
	if err := os.WriteFile(filepath.Join(held, "keep"), []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(held)
	if err != nil {
		t.Fatal(err)
	}
	dest := variantAudioTarget(t, f)
	variantBeforeTrackHook = func() {
		if err := os.Rename(dest, dest+".stranded"); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(held, dest); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { variantBeforeTrackHook = nil })
	dl := f.addDownload(t, "Dune by Frank Herbert [ENG / M4B]")
	f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "test-hash", "", nil, []string{source})
	variantBlocked(t, f, dl, "public directory changed")
	variantImportedFiles(t, f, 0)
	after, err := os.Lstat(dest)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("public replacement removed: %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(dest, "keep")); err != nil || string(contents) != sentinel {
		t.Fatalf("another writer's content changed: %q %v", contents, err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("torrent audio changed: %v", err)
	}
}

func TestVariantImport_MixedLanguageBundleImportsVerifiedEnglishOnly(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	english := variantEPUBSource(t, f.sources, "English catalogue label (English).epub", "english.epub")
	romanian := variantEPUBSource(t, f.sources, "English catalogue label.epub", "romanian-en-opf.epub")
	unknown := variantEPUBSource(t, f.sources, "English catalogue label (uncertain).epub", "mixed.epub")
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	author, err := db.NewAuthorRepo(f.database).GetByID(f.ctx, f.book.AuthorID)
	if err != nil {
		t.Fatal(err)
	}
	selected, reviewed, reason, err := f.scanner.reviewVariantImport(f.ctx, dl, f.book, author, models.MediaTypeEbook, f.sources, []string{romanian, unknown, english})
	if err != nil || reviewed || len(selected) != 1 || selected[0].path != english {
		t.Fatalf("mixed bundle selected wrong file: %+v reviewed=%t reason=%q err=%v", selected, reviewed, reason, err)
	}
	f.scanner.tryImportInternal(f.ctx, dl, f.sources, "qbittorrent", "test-hash", "", nil, []string{romanian, unknown, english})
	f.assertVariantPlaced(t, dl)
	if rejected, err := f.rejections.IsRejected(f.ctx, f.book.ID, f.variant.ID, romanian); err != nil || !rejected {
		t.Fatalf("confident wrong-language EPUB not rejected by exact file: %t %v", rejected, err)
	}
	if rejected, err := f.rejections.IsRejected(f.ctx, f.book.ID, f.variant.ID, english); err != nil || rejected {
		t.Fatalf("valid English sibling rejected: %t %v", rejected, err)
	}
	for _, source := range []string{romanian, unknown, english} {
		if _, err := os.Stat(source); err != nil {
			t.Fatalf("torrent source changed: %s: %v", source, err)
		}
	}
}
