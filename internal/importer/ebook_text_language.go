package importer

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/pemistahl/lingua-go"
)

const (
	maxTextZIPEntries   = 2000
	maxTextChapterBytes = 256 << 10
	maxTextTotalBytes   = 2 << 20
	maxTextChapters     = 8
	minTextChapterRunes = 1200
	minTextConfidence   = 0.85
)

// The detector compares against other Latin-script languages, not merely the
// three we can return. Otherwise a Spanish or Italian book could be declared
// English/French/Romanian with artificially high confidence. Models load lazily.
var ebookTextDetector = sync.OnceValue(func() lingua.LanguageDetector {
	return lingua.NewLanguageDetectorBuilder().FromAllLanguagesWithLatinScript().Build()
})

// DetectEbookTextLanguage samples bounded, interior XHTML from an EPUB without
// changing the source. It ignores OPF language claims; Task 7D will reconcile
// this evidence with metadata and decide whether an import needs review.
// A malformed or oversized archive can return an error, always without a claim.
func DetectEbookTextLanguage(filename string) (language string, confident bool, err error) {
	// archive/zip reads *all* directory records before exposing zr.File,
	// including when the count in the EOCD lies. Bound that walk first.
	if err := checkTextZIPDirectory(filename); err != nil {
		return "", false, err
	}
	zr, err := zip.OpenReader(filename)
	if err != nil {
		return "", false, fmt.Errorf("ebook language: open zip: %w", err)
	}
	defer zr.Close()
	if len(zr.File) > maxTextZIPEntries {
		return "", false, fmt.Errorf("ebook language: zip exceeds %d entries", maxTextZIPEntries)
	}

	opfPath, err := epubOPFPath(&zr.Reader)
	if err != nil {
		return "", false, err
	}
	opf := findZipFile(&zr.Reader, opfPath)
	if opf == nil {
		return "", false, fmt.Errorf("ebook language: missing OPF %q", opfPath)
	}
	r, err := openCappedEntry(opf)
	if err != nil {
		return "", false, fmt.Errorf("ebook language: open OPF: %w", err)
	}
	var pkg struct {
		Manifest []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			MediaType  string `xml:"media-type,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"manifest>item"`
		Spine []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"spine>itemref"`
	}
	parseErr := xml.NewDecoder(r).Decode(&pkg)
	closeErr := r.Close()
	if parseErr != nil {
		return "", false, fmt.Errorf("ebook language: parse OPF: %w", parseErr)
	}
	if closeErr != nil {
		return "", false, fmt.Errorf("ebook language: close OPF: %w", closeErr)
	}

	manifest := make(map[string]string, len(pkg.Manifest))
	for _, item := range pkg.Manifest {
		if item.ID == "" || item.Href == "" || item.MediaType != "application/xhtml+xml" || strings.Contains(strings.ToLower(item.Properties), "nav") || isTextFrontMatter(item.ID+"/"+item.Href) {
			continue
		}
		// EPUB manifest hrefs are URLs relative to the OPF, not filesystem
		// paths. Strip fragment/query and decode escaped path components.
		href := strings.SplitN(strings.SplitN(item.Href, "#", 2)[0], "?", 2)[0]
		href, err = url.PathUnescape(href)
		if err != nil || strings.HasPrefix(href, "/") {
			continue
		}
		manifest[item.ID] = path.Clean(path.Join(path.Dir(opfPath), href))
	}

	var sampled, qualified, bytesRead int
	var agreed string
	for _, ref := range pkg.Spine {
		chapterPath, eligible := manifest[ref.IDRef]
		if !eligible {
			continue
		}
		if sampled == maxTextChapters {
			break
		}
		sampled++
		chapter := findZipFile(&zr.Reader, chapterPath)
		if chapter == nil {
			return "", false, fmt.Errorf("ebook language: missing chapter %q", chapterPath)
		}
		text, n, err := readTextChapter(chapter, maxTextTotalBytes-bytesRead)
		bytesRead += n
		if err != nil {
			return "", false, err
		}
		if utf8.RuneCountInString(text) < minTextChapterRunes {
			continue
		}
		values := ebookTextDetector().ComputeLanguageConfidenceValues(text)
		if len(values) == 0 || values[0].Value() < minTextConfidence {
			return "", false, nil
		}
		var detected string
		switch values[0].Language() {
		case lingua.English:
			detected = "eng"
		case lingua.Romanian:
			detected = "rum"
		case lingua.French:
			detected = "fre"
		default:
			return "", false, nil
		}
		if agreed != "" && detected != agreed {
			return "", false, nil
		}
		agreed = detected
		qualified++
	}
	if qualified < 2 {
		return "", false, nil
	}
	return agreed, true, nil
}

// checkTextZIPDirectory walks at most 2000 central-directory headers before
// archive/zip sees them. ZIP64 and archives with a noncanonical central
// directory are unknown: this evidence reader need not accept every EPUB.
// This also rejects a forged small EOCD count with extra directory records.
func checkTextZIPDirectory(filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("ebook language: open zip: %w", err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("ebook language: stat zip: %w", err)
	}
	const endRecordSize = 22
	const maxCommentSize = 65535
	size := stat.Size()
	if size < endRecordSize {
		return fmt.Errorf("ebook language: no zip directory")
	}
	tail := make([]byte, min(size, endRecordSize+maxCommentSize))
	if _, err := f.ReadAt(tail, size-int64(len(tail))); err != nil {
		return fmt.Errorf("ebook language: read zip directory: %w", err)
	}
	end := -1
	for i := len(tail) - endRecordSize; i >= 0; i-- {
		if bytes.Equal(tail[i:i+4], []byte("PK\x05\x06")) && int(binary.LittleEndian.Uint16(tail[i+20:i+22])) == len(tail)-i-endRecordSize {
			end = i
			break
		}
	}
	if end < 0 {
		return fmt.Errorf("ebook language: no zip directory")
	}
	record := tail[end:]
	entries := binary.LittleEndian.Uint16(record[10:12])
	if entries > maxTextZIPEntries {
		return fmt.Errorf("ebook language: zip exceeds %d entries", maxTextZIPEntries)
	}
	dirSize := binary.LittleEndian.Uint32(record[12:16])
	dirOffset := binary.LittleEndian.Uint32(record[16:20])
	endOffset := size - int64(len(tail)) + int64(end)
	if binary.LittleEndian.Uint16(record[4:6]) != 0 || binary.LittleEndian.Uint16(record[6:8]) != 0 ||
		binary.LittleEndian.Uint16(record[8:10]) != entries || entries == 0xffff || dirSize == 0xffffffff || dirOffset == 0xffffffff ||
		int64(dirOffset)+int64(dirSize) != endOffset {
		return fmt.Errorf("ebook language: unsupported zip directory")
	}
	directory := io.NewSectionReader(f, int64(dirOffset), int64(dirSize))
	var count int
	for offset := int64(0); offset < int64(dirSize); count++ {
		if count == maxTextZIPEntries {
			return fmt.Errorf("ebook language: zip exceeds %d entries", maxTextZIPEntries)
		}
		var header [46]byte
		if _, err := io.ReadFull(directory, header[:]); err != nil || !bytes.Equal(header[:4], []byte("PK\x01\x02")) {
			return fmt.Errorf("ebook language: malformed zip directory")
		}
		recordSize := int64(len(header)) + int64(binary.LittleEndian.Uint16(header[28:30])) +
			int64(binary.LittleEndian.Uint16(header[30:32])) + int64(binary.LittleEndian.Uint16(header[32:34]))
		offset += recordSize
		if offset > int64(dirSize) {
			return fmt.Errorf("ebook language: malformed zip directory")
		}
		if _, err := directory.Seek(recordSize-int64(len(header)), io.SeekCurrent); err != nil {
			return fmt.Errorf("ebook language: seek zip directory: %w", err)
		}
	}
	if count != int(entries) {
		return fmt.Errorf("ebook language: malformed zip directory count")
	}
	return nil
}

// A declared size catches ordinary oversized ZIP entries before decompressing.
// Reject exact limit exhaustion as unknown too: this bounds decompression
// strictly without reading an extra byte to distinguish exact size from more.
// No oversized text reaches the XHTML parser or detector.
func readTextChapter(f *zip.File, remaining int) (string, int, error) {
	limit := min(maxTextChapterBytes, remaining)
	if limit <= 0 || f.UncompressedSize64 >= uint64(limit) {
		return "", 0, fmt.Errorf("ebook language: chapter %q exceeds decompression limit", f.Name)
	}
	r, err := f.Open()
	if err != nil {
		return "", 0, fmt.Errorf("ebook language: open chapter %q: %w", f.Name, err)
	}
	data, err := io.ReadAll(io.LimitReader(r, int64(limit)))
	closeErr := r.Close()
	if err != nil || closeErr != nil {
		return "", len(data), fmt.Errorf("ebook language: read chapter %q: %w", f.Name, errors.Join(err, closeErr))
	}
	if len(data) >= limit {
		return "", len(data), fmt.Errorf("ebook language: chapter %q exceeds decompression limit", f.Name)
	}
	text, err := xhtmlPlainText(data)
	if err != nil {
		return "", len(data), fmt.Errorf("ebook language: parse chapter %q: %w", f.Name, err)
	}
	return text, len(data), nil
}

// XML tokens avoid treating tags, attributes, scripts, styles or the document
// head as prose. EPUB chapter files are XHTML; malformed XML is unknown, not
// permission to guess a language from raw markup. XHTML named entities are
// accepted without loading network resources or external DTDs.
func xhtmlPlainText(data []byte) (string, error) {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Entity = xml.HTMLEntity
	var text strings.Builder
	var inBody bool
	var skipped int
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if skipped > 0 {
				skipped++
				continue
			}
			switch strings.ToLower(t.Name.Local) {
			case "head", "script", "style", "nav", "svg":
				skipped = 1
			case "body":
				inBody = true
			}
		case xml.EndElement:
			if skipped > 0 {
				skipped--
			} else if strings.EqualFold(t.Name.Local, "body") {
				inBody = false
			}
		case xml.CharData:
			if inBody && skipped == 0 {
				text.Write(t)
				text.WriteByte(' ')
			}
		}
	}
	return strings.Join(strings.Fields(text.String()), " "), nil
}

// Skip structural pages even when their (possibly long) XHTML is first in
// the spine. Interior filenames such as chapter-title.xhtml remain eligible.
func isTextFrontMatter(name string) bool {
	parts := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, part := range parts {
		switch part {
		case "title":
			if !strings.Contains(strings.ToLower(name), "chapter") {
				return true
			}
		case "cover", "coverpage", "titlepage", "front", "frontmatter", "nav", "toc", "contents", "copyright", "preface", "foreword", "dedication", "introduction", "colophon":
			return true
		}
	}
	return false
}
