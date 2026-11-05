package importer

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectEbookTextLanguage_TwoLongInteriorChaptersAgree(t *testing.T) {
	// Catches a default-to-English result, an OPF-only result, and a detector
	// that mistakes front matter for chapter evidence.
	for _, tc := range []struct{ file, want string }{
		{"english.epub", "eng"},
		{"french.epub", "fre"},
		{"romanian.epub", "rum"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			language, confident, err := DetectEbookTextLanguage(filepath.Join("testdata", tc.file))
			if err != nil || !confident || language != tc.want {
				t.Fatalf("got (%q, %t, %v); want (%q, true, nil)", language, confident, err, tc.want)
			}
		})
	}
}

func TestDetectEbookTextLanguage_RomanianTextOverrulesEnglishOPF(t *testing.T) {
	// Catches trusting a dc:language=en label above two Romanian chapters.
	language, confident, err := DetectEbookTextLanguage(filepath.Join("testdata", "romanian-en-opf.epub"))
	if err != nil || !confident || language != "rum" {
		t.Fatalf("got (%q, %t, %v); want (rum, true, nil)", language, confident, err)
	}
}

func TestDetectEbookTextLanguage_MixedChaptersAreUnknown(t *testing.T) {
	// Catches a detector claiming the first chapter's language for a mixed book.
	assertUnknownLanguage(t, filepath.Join("testdata", "mixed.epub"))
}

func TestDetectEbookTextLanguage_BilingualInteriorChaptersAreUnknown(t *testing.T) {
	// Catches a high-confidence but misleading single label for two chapters
	// each containing substantial English and Romanian prose.
	assertUnknownLanguage(t, filepath.Join("testdata", "intra-mixed.epub"))
}

func TestDetectEbookTextLanguage_UnsupportedSpanishIsUnknown(t *testing.T) {
	// Catches a detector restricted to three languages misidentifying Spanish.
	assertUnknownLanguage(t, filepath.Join("testdata", "spanish.epub"))
}

func TestDetectEbookTextLanguage_ShortChaptersAndFrontMatterDoNotQualify(t *testing.T) {
	// Catches counting XHTML markup or two long English front-matter pages.
	for _, name := range []string{"short.epub", "front-only.epub"} {
		t.Run(name, func(t *testing.T) { assertUnknownLanguage(t, filepath.Join("testdata", name)) })
	}
}

func TestDetectEbookTextLanguage_NonLinearAuxiliaryPagesDoNotQualify(t *testing.T) {
	// Two long page1/page2 files marked non-linear are not interior chapters.
	path := rewriteLanguageEPUB(t, "english.epub", func(entries map[string][]byte) {
		opf := string(entries["OEBPS/content.opf"])
		for i := 1; i <= 2; i++ {
			chapter := fmt.Sprintf("chapter%d.xhtml", i)
			page := fmt.Sprintf("page%d.xhtml", i)
			entries["OEBPS/"+page] = entries["OEBPS/"+chapter]
			delete(entries, "OEBPS/"+chapter)
			opf = strings.Replace(opf, "href='"+chapter+"'", "href='"+page+"'", 1)
			ref := fmt.Sprintf("<itemref idref='chapter%d'/>", i)
			opf = strings.Replace(opf, ref, fmt.Sprintf("<itemref idref='chapter%d' linear='no'/>", i), 1)
		}
		entries["OEBPS/content.opf"] = []byte(opf)
	})
	assertUnknownLanguage(t, path)
}

func TestDetectEbookTextLanguage_NonLinearAuxiliaryPagesDoNotOverrideChapters(t *testing.T) {
	// French auxiliary pages must not contradict two English linear chapters.
	path := rewriteLanguageEPUB(t, "english.epub", func(entries map[string][]byte) {
		opf := string(entries["OEBPS/content.opf"])
		for i := 1; i <= 2; i++ {
			page := fmt.Sprintf("page%d.xhtml", i)
			id := fmt.Sprintf("page%d", i)
			entries["OEBPS/"+page] = readFixtureChapter(t, "french.epub", fmt.Sprintf("chapter%d.xhtml", i))
			opf = strings.Replace(opf, "</manifest>", fmt.Sprintf("<item id='%s' href='%s' media-type='application/xhtml+xml'/></manifest>", id, page), 1)
			opf = strings.Replace(opf, "</spine>", fmt.Sprintf("<itemref idref='%s' linear='no'/></spine>", id), 1)
		}
		entries["OEBPS/content.opf"] = []byte(opf)
	})
	language, confident, err := DetectEbookTextLanguage(path)
	if err != nil || !confident || language != "eng" {
		t.Fatalf("got (%q, %t, %v); want (eng, true, nil)", language, confident, err)
	}
}

func TestDetectEbookTextLanguage_LongMarkupIsNotPlainText(t *testing.T) {
	// Catches counting XHTML attribute or tag runes toward the 1200-rune
	// minimum when each chapter has only a few visible words.
	path := rewriteLanguageEPUB(t, "english.epub", func(entries map[string][]byte) {
		markup := "<html><body>" + strings.Repeat("<span data-note='A quiet garden in the morning sunlight'></span>", 100) + "<p>Short page.</p></body></html>"
		entries["OEBPS/chapter1.xhtml"] = []byte(markup)
		entries["OEBPS/chapter2.xhtml"] = []byte(markup)
	})
	assertUnknownLanguage(t, path)
}

func TestDetectEbookTextLanguage_MalformedZIPNeverClaimsLanguage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.epub")
	if err := os.WriteFile(path, []byte("not a zip file"), 0600); err != nil {
		t.Fatal(err)
	}
	assertUnknownLanguage(t, path)
}

func TestDetectEbookTextLanguage_OversizedDeclaredChapterNeverClaimsLanguage(t *testing.T) {
	// A compressible chapter advertises >256 KiB decompressed. Its companion
	// chapter is good, so an unchecked oversized first chapter could claim English.
	path := rewriteLanguageEPUB(t, "english.epub", func(entries map[string][]byte) {
		entries["OEBPS/chapter1.xhtml"] = []byte("<html><body><p>" + strings.Repeat("English garden sunlight and morning visitors. ", 7500) + "</p></body></html>")
	})
	assertUnknownLanguage(t, path)
}

func TestDetectEbookTextLanguage_ChapterAtByteLimitIsUnknown(t *testing.T) {
	// Exhaustion at exactly 256 KiB must not read one extra decompressed byte
	// to decide whether there is more. Conservatively decline the chapter.
	path := rewriteLanguageEPUB(t, "english.epub", func(entries map[string][]byte) {
		prefix, suffix := "<html><body><p>", "</p></body></html>"
		phrase := "English garden sunlight and morning visitors. "
		length := 256<<10 - len(prefix) - len(suffix)
		body := strings.Repeat(phrase, length/len(phrase)) + strings.Repeat("a", length%len(phrase))
		entries["OEBPS/chapter1.xhtml"] = []byte(prefix + body + suffix)
	})
	assertUnknownLanguage(t, path)
}

func TestDetectEbookTextLanguage_TooManyZIPEntriesNeverClaimsLanguage(t *testing.T) {
	// A valid English book with >2000 extra entries must not be sampled.
	path := rewriteLanguageEPUB(t, "english.epub", func(entries map[string][]byte) {
		for i := 0; i < 2001; i++ {
			entries[fmt.Sprintf("noise/%04d.txt", i)] = []byte("x")
		}
	})
	assertUnknownLanguage(t, path)
}

func TestDetectEbookTextLanguage_LargeZIPDirectoryIsRejectedBeforeParsing(t *testing.T) {
	// Fewer than 2000 records can still consume megabytes of names and comments.
	source, err := zip.OpenReader(filepath.Join("testdata", "english.epub"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	path := filepath.Join(t.TempDir(), "large-directory.epub")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	for _, entry := range source.File {
		if err := zw.Copy(entry); err != nil {
			t.Fatal(err)
		}
	}
	comment := strings.Repeat("x", 60_000)
	for i := 0; i < 150; i++ {
		_, err := zw.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("noise/%03d.txt", i), Comment: comment, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	language, confident, err := DetectEbookTextLanguage(path)
	if language != "" || confident || err == nil || !strings.Contains(err.Error(), "zip directory exceeds") {
		t.Fatalf("got (%q, %t, %v); want a preflight directory-byte-limit error", language, confident, err)
	}
}

func TestDetectEbookTextLanguage_ForgedLargeZIPDirectoryIsRejectedBeforeParsing(t *testing.T) {
	// Reject a claimed huge directory even when the archive cannot hold it.
	path := rewriteLanguageEPUB(t, "english.epub", func(map[string][]byte) {})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	eocd := strings.LastIndex(string(data), "PK\x05\x06")
	if eocd < 0 {
		t.Fatal("fixture ZIP has no end-of-central-directory record")
	}
	binary.LittleEndian.PutUint32(data[eocd+12:eocd+16], 9<<20)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	language, confident, err := DetectEbookTextLanguage(path)
	if language != "" || confident || err == nil || !strings.Contains(err.Error(), "zip directory exceeds") {
		t.Fatalf("got (%q, %t, %v); want a preflight directory-byte-limit error", language, confident, err)
	}
}

func TestDetectEbookTextLanguage_LyingZIPEntryCountIsRejectedBeforeParsing(t *testing.T) {
	// archive/zip loads every central-directory record even if the EOCD count
	// claims otherwise. Reject the actual 2001+ records before it allocates.
	path := rewriteLanguageEPUB(t, "english.epub", func(entries map[string][]byte) {
		for i := 0; i < 2001; i++ {
			entries[fmt.Sprintf("noise/%04d.txt", i)] = []byte("x")
		}
	})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	eocd := strings.LastIndex(string(data), "PK\x05\x06")
	if eocd < 0 {
		t.Fatal("fixture ZIP has no end-of-central-directory record")
	}
	data[eocd+8], data[eocd+9] = 1, 0   // records on disk
	data[eocd+10], data[eocd+11] = 1, 0 // records total
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	language, confident, err := DetectEbookTextLanguage(path)
	if language != "" || confident || err == nil || !strings.Contains(err.Error(), "exceeds 2000 entries") {
		t.Fatalf("got (%q, %t, %v); want a preflight entry-limit error", language, confident, err)
	}
}

func TestDetectEbookTextLanguage_RepeatedSpineIDRefIsOneChapter(t *testing.T) {
	// Repeating an idref must not turn one long XHTML file into two chapters.
	path := rewriteLanguageEPUB(t, "english.epub", func(entries map[string][]byte) {
		opf := string(entries["OEBPS/content.opf"])
		entries["OEBPS/content.opf"] = []byte(strings.Replace(opf, "<itemref idref='chapter2'/>", "<itemref idref='chapter1'/>", 1))
	})
	assertUnknownLanguage(t, path)
}

func TestDetectEbookTextLanguage_ManifestAliasesAreOneChapter(t *testing.T) {
	// Separate manifest IDs that resolve to the same file are not independent evidence.
	path := rewriteLanguageEPUB(t, "english.epub", func(entries map[string][]byte) {
		opf := string(entries["OEBPS/content.opf"])
		entries["OEBPS/content.opf"] = []byte(strings.Replace(opf, "href='chapter2.xhtml'", "href='chapter1.xhtml'", 1))
	})
	assertUnknownLanguage(t, path)
}

func TestDetectEbookTextLanguage_RepeatedRefsDoNotHideLaterDisagreement(t *testing.T) {
	// Repeated paths cannot consume the eight distinct-chapter sample slots.
	path := rewriteLanguageEPUB(t, "english.epub", func(entries map[string][]byte) {
		entries["OEBPS/chapter2.xhtml"] = readFixtureChapter(t, "romanian.epub", "chapter1.xhtml")
		opf := string(entries["OEBPS/content.opf"])
		entries["OEBPS/content.opf"] = []byte(strings.Replace(opf, "<itemref idref='chapter2'/>", strings.Repeat("<itemref idref='chapter1'/>", 8)+"<itemref idref='chapter2'/>", 1))
	})
	assertUnknownLanguage(t, path)
}

func TestDetectEbookTextLanguage_LaterDisagreeingChapterIsUnknown(t *testing.T) {
	// Catches stopping at the first two agreeing chapters despite a mixed book.
	path := rewriteLanguageEPUB(t, "english.epub", func(entries map[string][]byte) {
		romanian := readFixtureChapter(t, "romanian.epub", "chapter1.xhtml")
		addLanguageChapter(t, entries, 3, romanian)
	})
	assertUnknownLanguage(t, path)
}

func TestDetectEbookTextLanguage_ChapterTitleFilenameIsInterior(t *testing.T) {
	// Catches a structural-page filter that drops a chapter merely because
	// the chapter filename contains the word "title".
	path := rewriteLanguageEPUB(t, "english.epub", func(entries map[string][]byte) {
		entries["OEBPS/chapter-title.xhtml"] = entries["OEBPS/chapter1.xhtml"]
		delete(entries, "OEBPS/chapter1.xhtml")
		entries["OEBPS/content.opf"] = []byte(strings.Replace(string(entries["OEBPS/content.opf"]), "href='chapter1.xhtml'", "href='chapter-title.xhtml'", 1))
	})
	language, confident, err := DetectEbookTextLanguage(path)
	if err != nil || !confident || language != "eng" {
		t.Fatalf("got (%q, %t, %v); want (eng, true, nil)", language, confident, err)
	}
}

func TestDetectEbookTextLanguage_NinthChapterIsOutsideSample(t *testing.T) {
	// Catches reading unbounded chapters (and exceeding the 2 MiB sample cap).
	path := rewriteLanguageEPUB(t, "english.epub", func(entries map[string][]byte) {
		english := entries["OEBPS/chapter1.xhtml"]
		for i := 3; i <= 8; i++ {
			addLanguageChapter(t, entries, i, english)
		}
		addLanguageChapter(t, entries, 9, readFixtureChapter(t, "romanian.epub", "chapter1.xhtml"))
	})
	language, confident, err := DetectEbookTextLanguage(path)
	if err != nil || !confident || language != "eng" {
		t.Fatalf("got (%q, %t, %v); want (eng, true, nil) from first eight", language, confident, err)
	}
}

func TestDetectEbookTextLanguage_DoesNotModifySource(t *testing.T) {
	// Catches rewriting a source EPUB, even when its OPF disagrees with text.
	source, err := os.ReadFile(filepath.Join("testdata", "romanian-en-opf.epub"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "seeded.epub")
	if err := os.WriteFile(path, source, 0600); err != nil {
		t.Fatal(err)
	}
	before := sha256.Sum256(source)
	language, confident, err := DetectEbookTextLanguage(path)
	if err != nil || !confident || language != "rum" {
		t.Fatalf("got (%q, %t, %v); want (rum, true, nil)", language, confident, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(after) != before {
		t.Fatal("source EPUB changed during read-only detection")
	}
}

func assertUnknownLanguage(t *testing.T, path string) {
	t.Helper()
	language, confident, err := DetectEbookTextLanguage(path)
	if language != "" || confident {
		t.Fatalf("got (%q, %t, %v); want (\"\", false, error or nil)", language, confident, err)
	}
}

func readFixtureChapter(t *testing.T, name, chapter string) []byte {
	t.Helper()
	zr, err := zip.OpenReader(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/"+chapter) {
			r, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(r)
			r.Close()
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
	}
	t.Fatalf("fixture chapter %s not found", chapter)
	return nil
}

func addLanguageChapter(t *testing.T, entries map[string][]byte, number int, data []byte) {
	t.Helper()
	name := fmt.Sprintf("chapter%d.xhtml", number)
	id := fmt.Sprintf("chapter%d", number)
	entries["OEBPS/"+name] = data
	opf := string(entries["OEBPS/content.opf"])
	opf = strings.Replace(opf, "</manifest>", fmt.Sprintf("<item id='%s' href='%s' media-type='application/xhtml+xml'/></manifest>", id, name), 1)
	opf = strings.Replace(opf, "</spine>", fmt.Sprintf("<itemref idref='%s'/></spine>", id), 1)
	entries["OEBPS/content.opf"] = []byte(opf)
}

func rewriteLanguageEPUB(t *testing.T, original string, mutate func(map[string][]byte)) string {
	t.Helper()
	zr, err := zip.OpenReader(filepath.Join("testdata", original))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	entries := make(map[string][]byte)
	for _, f := range zr.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		entries[f.Name] = data
	}
	mutate(entries)
	path := filepath.Join(t.TempDir(), "test.epub")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
