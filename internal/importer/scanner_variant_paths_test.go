package importer

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

func addVariantDownload(t *testing.T, f *variantReviewFixture, title string, variantID int64) *models.Download {
	t.Helper()
	dl := &models.Download{GUID: "variant-test-" + title, Title: title, BookID: &f.book.ID, VariantID: &variantID, Status: models.StateCompleted, Protocol: "torrent"}
	if err := f.downloads.Create(f.ctx, dl); err != nil {
		t.Fatal(err)
	}
	return dl
}

func variantImportedFiles(t *testing.T, f *variantReviewFixture, count int) []models.BookFile {
	t.Helper()
	rows, err := f.books.ListFiles(f.ctx, f.book.ID)
	if err != nil || len(rows) != count {
		t.Fatalf("book files = %+v, %v; want %d", rows, err, count)
	}
	return rows
}

func variantImportedDownload(t *testing.T, f *variantReviewFixture, id int64) {
	t.Helper()
	got, err := f.downloads.GetByID(f.ctx, id)
	if err != nil || got.Status != models.StateImported {
		t.Fatalf("download not imported: %+v %v", got, err)
	}
}

func TestVariantHardlink_TwoEbookLanguagesKeepSourcesAndLegacyPath(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	other, err := f.books.VariantRepo().EnsureWanted(f.ctx, f.book.ID, models.MediaTypeEbook, []string{"rum"})
	if err != nil || len(other) != 1 {
		t.Fatalf("wanted Romanian: %+v %v", other, err)
	}
	legacy := filepath.Join(f.library, "legacy.epub")
	if err := os.WriteFile(legacy, []byte("old edition"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.books.AddBookFile(f.ctx, f.book.ID, models.MediaTypeEbook, legacy); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		lang, fixture string
		variantID     int64
	}{
		{"eng", "english.epub", f.variant.ID},
		{"rum", "romanian-en-opf.epub", other[0].ID},
	} {
		source := variantEPUBSource(t, f.sources, "English catalogue label ("+tc.lang+").epub", tc.fixture)
		before, err := os.Lstat(source)
		if err != nil {
			t.Fatal(err)
		}
		dl := addVariantDownload(t, f, "English catalogue label ["+strings.ToUpper(tc.lang)+" / EPUB]", tc.variantID)
		f.importSource(t, dl, source)
		variantImportedDownload(t, f, dl.ID)
		rows := variantImportedFiles(t, f, map[string]int{"eng": 2, "rum": 3}[tc.lang])
		found := false
		for _, row := range rows {
			if row.VariantID == nil || *row.VariantID != tc.variantID {
				continue
			}
			if !strings.Contains(row.Path, " ["+tc.lang+"].epub") || row.Language != tc.lang || row.Verification != "verified" {
				t.Fatalf("incorrect file evidence or name: %+v", row)
			}
			linked, err := os.Lstat(row.Path)
			if err != nil || !os.SameFile(before, linked) {
				t.Fatalf("not a source hardlink: %+v %v", row, err)
			}
			if st, ok := linked.Sys().(*syscall.Stat_t); !ok || st.Nlink < 2 {
				t.Fatalf("missing source hardlink count: %+v", linked)
			}
			found = true
		}
		if !found {
			t.Fatalf("missing %s variant file: %+v", tc.lang, rows)
		}
		after, err := os.Lstat(source)
		if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			t.Fatalf("seeded source changed: %v", err)
		}
	}
	rows := variantImportedFiles(t, f, 3)
	if rows[0].Path != legacy || rows[0].VariantID != nil || rows[0].Verification != "unverified" {
		t.Fatalf("legacy association changed: %+v", rows[0])
	}
	english, _ := os.Lstat(rows[1].Path)
	romanian, _ := os.Lstat(rows[2].Path)
	if english == nil || romanian == nil || os.SameFile(english, romanian) {
		t.Fatalf("languages share the same inode: %+v", rows)
	}
	book, err := f.books.GetByID(f.ctx, f.book.ID)
	if err != nil || book.Language != "" {
		t.Fatalf("variant OPF rewrote catalogue language: %+v %v", book, err)
	}
}

func TestVariantHardlink_MixedBundleFillsOnlyVerifiedRequestedLanguages(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	other, err := f.books.VariantRepo().EnsureWanted(f.ctx, f.book.ID, models.MediaTypeEbook, []string{"rum"})
	if err != nil || len(other) != 1 {
		t.Fatal(err)
	}
	english := variantEPUBSource(t, f.sources, "English catalogue label (English).epub", "english.epub")
	romanian := variantEPUBSource(t, f.sources, "English catalogue label (Romanian).epub", "romanian-en-opf.epub")
	unknown := variantEPUBSource(t, f.sources, "English catalogue label (Unknown).epub", "mixed.epub")
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	f.scanner.tryImportInternal(f.ctx, dl, f.sources, "qbittorrent", "test-hash", "", nil, []string{english, romanian, unknown})
	variantImportedDownload(t, f, dl.ID)
	rows := variantImportedFiles(t, f, 2)
	found := map[int64]bool{}
	for _, row := range rows {
		if row.VariantID == nil || row.Verification != "verified" || row.Language == "" {
			t.Fatalf("unknown attached or language invented: %+v", row)
		}
		linked, err := os.Lstat(row.Path)
		if err != nil {
			t.Fatal(err)
		}
		if row.Language == "eng" && !sameFilePath(t, english, linked) || row.Language == "rum" && !sameFilePath(t, romanian, linked) {
			t.Fatalf("wrong source for variant: %+v", row)
		}
		found[*row.VariantID] = true
	}
	if !found[f.variant.ID] || !found[other[0].ID] {
		t.Fatalf("missing requested variant: %+v", rows)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("unknown seed changed: %v", err)
	}
}

func sameFilePath(t *testing.T, path string, info os.FileInfo) bool {
	t.Helper()
	original, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(original, info)
}

func TestVariantHardlink_MixedBundleFillsAlreadyDownloadingSibling(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	other, err := f.books.VariantRepo().EnsureWanted(f.ctx, f.book.ID, models.MediaTypeEbook, []string{"rum"})
	if err != nil || len(other) != 1 {
		t.Fatal(err)
	}
	if err := f.books.VariantRepo().SetStatus(f.ctx, other[0].ID, models.VariantDownloading); err != nil {
		t.Fatal(err)
	}
	english := variantEPUBSource(t, f.sources, "English catalogue label (English).epub", "english.epub")
	romanian := variantEPUBSource(t, f.sources, "English catalogue label (Romanian).epub", "romanian-en-opf.epub")
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	f.scanner.tryImportInternal(f.ctx, dl, f.sources, "qbittorrent", "hash", "", nil, []string{english, romanian})
	variantImportedDownload(t, f, dl.ID)
	rows := variantImportedFiles(t, f, 2)
	for _, row := range rows {
		if row.VariantID == nil || row.Verification != "verified" {
			t.Fatalf("wrong claim: %+v", row)
		}
	}
}

func TestVariantHardlink_VerifiedSiblingDoesNotCompleteUnverifiedPrimary(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	other, err := f.books.VariantRepo().EnsureWanted(f.ctx, f.book.ID, models.MediaTypeEbook, []string{"rum"})
	if err != nil || len(other) != 1 {
		t.Fatal(err)
	}
	unknown := variantEPUBSource(t, f.sources, "English catalogue label (unknown).epub", "mixed.epub")
	romanian := variantEPUBSource(t, f.sources, "English catalogue label (Romanian).epub", "romanian-en-opf.epub")
	dl := f.addDownload(t, "English catalogue label [EPUB]")
	f.scanner.tryImportInternal(f.ctx, dl, f.sources, "qbittorrent", "test-hash", "", nil, []string{unknown, romanian})
	variantBlocked(t, f, dl, "requested variant remains wanted")
	rows := variantImportedFiles(t, f, 1)
	if rows[0].VariantID == nil || *rows[0].VariantID != other[0].ID || rows[0].Language != "rum" || rows[0].Verification != "verified" {
		t.Fatalf("unverified primary claimed or sibling lost: %+v", rows)
	}
	variants, err := f.books.VariantRepo().ListByBook(f.ctx, f.book.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range variants {
		if v.ID == f.variant.ID && v.Status != models.VariantWanted {
			t.Fatalf("English variant falsely completed: %+v", v)
		}
	}
}

func TestVariantHardlink_UnknownEbookKeepsEmptyDetectedLanguage(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "mixed.epub")
	dl := f.addDownload(t, "English catalogue label [EPUB]")
	f.importSource(t, dl, source)
	variantImportedDownload(t, f, dl.ID)
	rows := variantImportedFiles(t, f, 1)
	if rows[0].Language != "" || rows[0].Verification != "unverified" || rows[0].VariantID == nil || *rows[0].VariantID != f.variant.ID {
		t.Fatalf("unverified file falsely labeled: %+v", rows[0])
	}
}

func TestVariantHardlink_AudioLanguagesPromoteSeparateWholeFolders(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
	if _, err := f.database.ExecContext(f.ctx, `UPDATE books SET release_date='1965-01-01T00:00:00Z' WHERE id=?`, f.book.ID); err != nil {
		t.Fatal(err)
	}
	other, err := f.books.VariantRepo().EnsureWanted(f.ctx, f.book.ID, models.MediaTypeAudiobook, []string{"rum"})
	if err != nil || len(other) != 1 {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		lang, title string
		variantID   int64
	}{
		{"eng", "Dune", f.variant.ID}, {"rum", "Dune", other[0].ID},
	} {
		folder := filepath.Join(f.sources, tc.lang)
		if err := os.Mkdir(folder, 0o700); err != nil {
			t.Fatal(err)
		}
		files := []string{filepath.Join(folder, "01.m4b"), filepath.Join(folder, "02.m4b")}
		for _, p := range files {
			if err := os.WriteFile(p, mp4File(atom("\xa9alb", dataAtom(1, []byte(tc.title))), atom("\xa9ART", dataAtom(1, []byte("Frank Herbert"))), atom("\xa9lan", dataAtom(1, []byte(tc.lang)))), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		dl := addVariantDownload(t, f, "Dune by Frank Herbert ["+strings.ToUpper(tc.lang)+" / M4B]", tc.variantID)
		f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "test-hash", "", nil, files)
		variantImportedDownload(t, f, dl.ID)
		rows := variantImportedFiles(t, f, map[string]int{"eng": 1, "rum": 2}[tc.lang])
		var dest string
		for _, row := range rows {
			if row.VariantID != nil && *row.VariantID == tc.variantID {
				dest = row.Path
			}
		}
		if filepath.Base(dest) != "Dune (1965) ["+tc.lang+"]" {
			t.Fatalf("audio folder lacks year or variant: %q", dest)
		}
		for _, row := range rows {
			if row.VariantID != nil && *row.VariantID == tc.variantID && (row.Language != "" || row.Verification != "unverified") {
				t.Fatalf("audio tags claimed a speech language: %+v", row)
			}
		}
		for _, source := range files {
			linked, err := os.Lstat(filepath.Join(dest, filepath.Base(source)))
			if err != nil || !sameFilePath(t, source, linked) {
				t.Fatalf("audio not hardlinked: %s: %v", source, err)
			}
		}
	}
}
