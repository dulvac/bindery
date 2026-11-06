//go:build !windows

package importer

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// A legacy download has no variant_id, so a variant-only mode guard leaves its
// still-seeding qBittorrent source vulnerable to mutable import settings.
func TestLegacyQbitImport_AlwaysHardlinksDespiteModeAndCategoryDrift(t *testing.T) {
	for _, mode := range []string{"move", "copy", "external"} {
		for _, categories := range qbitCategoryMatrix {
			t.Run(mode+"/configured="+categories.configured+"/torrent="+categories.torrent, func(t *testing.T) {
				library := t.TempDir()
				s, book, downloads, books, settings, _, ctx := sharedFormatFixture(t, library)
				if err := settings.Set(ctx, "import.mode", mode); err != nil {
					t.Fatal(err)
				}
				drop := t.TempDir()
				if err := settings.Set(ctx, "import.drop_folder", drop); err != nil {
					t.Fatal(err)
				}
				sourceDir := t.TempDir()
				name := "We Who Wrestle with God.epub"
				source := filepath.Join(sourceDir, name)
				original := bookSized("seed data must remain unchanged")
				if err := os.WriteFile(source, original, 0o644); err != nil {
					t.Fatal(err)
				}
				before, err := os.Stat(source)
				if err != nil {
					t.Fatal(err)
				}
				fake := &fakeQbit{t: t, torrent: map[string]any{
					"hash": removeOnImportHash, "name": name, "state": "uploading",
					"progress": 1.0, "amount_left": 0, "save_path": sourceDir,
					"content_path": source, "category": categories.torrent,
				}, files: []map[string]any{{"index": 0, "name": name, "size": len(original), "progress": 1}}}
				srv := httptest.NewServer(fake)
				defer srv.Close()
				host, port := scannerTestHostPort(t, srv.URL)
				client := &models.DownloadClient{Type: "qbittorrent", Host: host, Port: port, Category: categories.configured, RemoveOnImport: true, Enabled: true}
				if err := s.clients.Create(ctx, client); err != nil {
					t.Fatal(err)
				}
				hash := removeOnImportHash
				dl := &models.Download{GUID: "legacy-" + mode + categories.configured + categories.torrent, Title: book.Title, BookID: &book.ID, DownloadClientID: &client.ID, TorrentID: &hash, Status: models.StateDownloading, Protocol: "torrent"}
				if err := downloads.Create(ctx, dl); err != nil {
					t.Fatal(err)
				}
				if dl.VariantID != nil {
					t.Fatal("precondition: legacy download must have variant_id=NULL")
				}

				s.checkQbittorrentDownloads(ctx, client)

				got, err := downloads.GetByID(ctx, dl.ID)
				if err != nil || got.Status != models.StateImported {
					t.Fatalf("import status = %+v, %v; want imported", got, err)
				}
				files, err := books.ListFiles(ctx, book.ID)
				if err != nil || len(files) != 1 {
					t.Fatalf("library files = %+v, %v; want one hardlink", files, err)
				}
				linked, err := os.Stat(files[0].Path)
				if err != nil || !os.SameFile(before, linked) {
					t.Fatalf("library file is not a hardlink of the original source: %v", err)
				}
				after, err := os.Stat(source)
				if err != nil || !os.SameFile(before, after) {
					t.Fatalf("source inode changed: %v", err)
				}
				contents, err := os.ReadFile(source)
				if err != nil || !bytes.Equal(contents, original) {
					t.Fatalf("source bytes changed: %v", err)
				}
				entries, err := os.ReadDir(drop)
				if err != nil || len(entries) != 0 {
					t.Fatalf("external drop folder received seeded file: %v, %v", entries, err)
				}
				if reqs := fake.deleteRequests(); len(reqs) != 0 {
					t.Fatalf("client received %d delete request(s)", len(reqs))
				}
			})
		}
	}
}

func TestLegacyQbitImport_HardlinkFailureBlocksWithoutCopyOrRemoval(t *testing.T) {
	for _, mediaType := range []string{models.MediaTypeEbook, models.MediaTypeAudiobook} {
		t.Run(mediaType, func(t *testing.T) {
			library := t.TempDir()
			s, book, downloads, books, settings, _, ctx := sharedFormatFixture(t, library)
			book.MediaType = mediaType
			if err := books.Update(ctx, book); err != nil {
				t.Fatal(err)
			}
			if err := settings.Set(ctx, "import.mode", "copy"); err != nil {
				t.Fatal(err)
			}
			if err := settings.Set(ctx, "import.audiobook.mode", "move"); err != nil {
				t.Fatal(err)
			}
			sourceDir := t.TempDir()
			name := "We Who Wrestle with God.epub"
			data := bookSized("original source")
			if mediaType == models.MediaTypeAudiobook {
				name = "01.m4b"
				data = mp4File(atom("\xa9alb", dataAtom(1, []byte(book.Title))), atom("\xa9ART", dataAtom(1, []byte("Jordan B. Peterson"))))
			}
			source := filepath.Join(sourceDir, name)
			if err := os.WriteFile(source, data, 0o644); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			fake := &fakeQbit{t: t, torrent: map[string]any{
				"hash": removeOnImportHash, "name": name, "state": "uploading",
				"progress": 1.0, "amount_left": 0, "save_path": sourceDir,
				"content_path": source, "category": "books",
			}, files: []map[string]any{{"index": 0, "name": name, "size": len(data), "progress": 1}}}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			host, port := scannerTestHostPort(t, srv.URL)
			client := &models.DownloadClient{Type: "qbittorrent", Host: host, Port: port, Category: "mam", RemoveOnImport: true, Enabled: true}
			if err := s.clients.Create(ctx, client); err != nil {
				t.Fatal(err)
			}
			hash := removeOnImportHash
			dl := &models.Download{GUID: "legacy-hardlink-failure-" + mediaType, Title: book.Title, BookID: &book.ID, DownloadClientID: &client.ID, TorrentID: &hash, Status: models.StateDownloading, Protocol: "torrent"}
			if err := downloads.Create(ctx, dl); err != nil {
				t.Fatal(err)
			}
			forceEXDEV(t)

			s.checkQbittorrentDownloads(ctx, client)

			got, err := downloads.GetByID(ctx, dl.ID)
			if err != nil || got.Status != models.StateImportBlocked || !strings.Contains(got.ErrorMessage, "hardlink") {
				t.Fatalf("hardlink failure must block: %+v, %v", got, err)
			}
			if rows, err := books.ListFiles(ctx, book.ID); err != nil || len(rows) != 0 {
				t.Fatalf("copied library files after link failure: %+v, %v", rows, err)
			}
			after, err := os.Stat(source)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("source inode changed: %v", err)
			}
			contents, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(contents, data) {
				t.Fatalf("source bytes changed: %v", err)
			}
			if reqs := fake.deleteRequests(); len(reqs) != 0 {
				t.Fatalf("client received %d delete request(s)", len(reqs))
			}
		})
	}
}

func TestLegacyQbitAudioMerge_LinkFailureLeavesPartialLinksForReconciliation(t *testing.T) {
	library := t.TempDir()
	s, book, downloads, books, settings, _, ctx := sharedFormatFixture(t, library)
	if err := settings.Set(ctx, "import.mode", "copy"); err != nil {
		t.Fatal(err)
	}
	if err := settings.Set(ctx, "import.audiobook.mode", "move"); err != nil {
		t.Fatal(err)
	}
	ebook := filepath.Join(t.TempDir(), "We Who Wrestle with God.epub")
	if err := os.WriteFile(ebook, bookSized("existing library ebook"), 0o644); err != nil {
		t.Fatal(err)
	}
	initial := &models.Download{GUID: "preexisting-ebook", Title: book.Title, BookID: &book.ID, Status: models.StateCompleted}
	if err := downloads.Create(ctx, initial); err != nil {
		t.Fatal(err)
	}
	s.tryImportInternal(ctx, initial, ebook, "transmission", "hash", "", nil, []string{ebook})
	rows, err := books.ListFiles(ctx, book.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("precondition: ebook not imported: %+v, %v", rows, err)
	}
	publicDir := filepath.Dir(rows[0].Path)
	originalEbook, err := os.ReadFile(rows[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	audioDir := filepath.Join(t.TempDir(), "recording")
	if err := os.Mkdir(audioDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var audio []string
	var infos []os.FileInfo
	for _, name := range []string{"01.m4b", "02.m4b"} {
		path := filepath.Join(audioDir, name)
		data := mp4File(atom("\xa9alb", dataAtom(1, []byte(book.Title))), atom("\xa9ART", dataAtom(1, []byte("Jordan B. Peterson"))))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		audio, infos = append(audio, path), append(infos, info)
	}
	qbit := &models.Download{GUID: "legacy-qbit-audio-merge", Title: book.Title, BookID: &book.ID, Status: models.StateCompleted, Protocol: "torrent"}
	if err := downloads.Create(ctx, qbit); err != nil {
		t.Fatal(err)
	}
	originalLink := osLink
	links := 0
	osLink = func(src, dst string) error {
		links++
		if links == 2 {
			return &os.LinkError{Op: "link", Old: src, New: dst, Err: syscall.EXDEV}
		}
		return originalLink(src, dst)
	}
	t.Cleanup(func() { osLink = originalLink })

	s.tryImportInternal(withTorrentOwnFolder(ctx, true), qbit, audioDir, "qbittorrent", "hash", "", nil, audio)

	got, err := downloads.GetByID(ctx, qbit.ID)
	if err != nil || got.Status != models.StateImportBlocked || !strings.Contains(got.ErrorMessage, "hardlink") || !strings.Contains(got.ErrorMessage, "reconcile") || !strings.Contains(got.ErrorMessage, publicDir) {
		t.Fatalf("second hardlink failure must block with public reconciliation path: %+v, %v", got, err)
	}
	if b, err := os.ReadFile(rows[0].Path); err != nil || !bytes.Equal(b, originalEbook) {
		t.Fatalf("existing ebook modified: %v", err)
	}
	placed := 0
	for i, path := range audio {
		if linked, err := os.Stat(filepath.Join(publicDir, filepath.Base(path))); err == nil {
			if !os.SameFile(infos[i], linked) {
				t.Errorf("public audio is not the seeded inode: %s", path)
			}
			placed++
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat public audio: %v", err)
		}
		if after, err := os.Stat(path); err != nil || !os.SameFile(infos[i], after) {
			t.Fatalf("seed file changed: %v", err)
		}
	}
	if placed != 1 {
		t.Errorf("placed %d links, want one partial link left for reconciliation", placed)
	}
}

func TestLegacyQbitImport_PersistedClientIdentityOverridesCallerLabel(t *testing.T) {
	for _, callerType := range []string{"", "transmission"} {
		t.Run("caller="+callerType, func(t *testing.T) {
			library := t.TempDir()
			s, book, downloads, books, settings, _, ctx := sharedFormatFixture(t, library)
			if err := settings.Set(ctx, "import.mode", "move"); err != nil {
				t.Fatal(err)
			}
			client := &models.DownloadClient{Type: "qbittorrent", Name: "protected source"}
			if err := s.clients.Create(ctx, client); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(t.TempDir(), "We Who Wrestle with God.epub")
			if err := os.WriteFile(source, bookSized("stored qbit client"), 0o644); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			dl := &models.Download{GUID: "persisted-qbit-" + callerType, Title: book.Title, BookID: &book.ID, DownloadClientID: &client.ID, Status: models.StateCompleted, Protocol: "torrent"}
			if err := downloads.Create(ctx, dl); err != nil {
				t.Fatal(err)
			}
			s.tryImportInternal(ctx, dl, source, callerType, "hash", "", nil, []string{source})
			got, err := downloads.GetByID(ctx, dl.ID)
			if err != nil || got.Status != models.StateImported {
				t.Fatalf("import status = %+v, %v", got, err)
			}
			rows, err := books.ListFiles(ctx, book.ID)
			if err != nil || len(rows) != 1 {
				t.Fatalf("library files = %+v, %v", rows, err)
			}
			linked, err := os.Stat(rows[0].Path)
			if err != nil || !os.SameFile(before, linked) {
				t.Fatalf("persisted qbit source copied or moved: %v", err)
			}
			if after, err := os.Stat(source); err != nil || !os.SameFile(before, after) {
				t.Fatalf("persisted qbit source missing: %v", err)
			}
		})
	}
}

func TestLegacyQbitImport_MissingProtocolStillProtectsTorrent(t *testing.T) {
	library := t.TempDir()
	s, book, downloads, books, settings, _, ctx := sharedFormatFixture(t, library)
	if err := settings.Set(ctx, "import.mode", "move"); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "We Who Wrestle with God.epub")
	if err := os.WriteFile(source, bookSized("legacy protocol missing"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	dl := &models.Download{GUID: "legacy-qbit-protocol-blank", Title: book.Title, BookID: &book.ID, Status: models.StateCompleted}
	if err := downloads.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	s.tryImportInternal(ctx, dl, source, "qbittorrent", "test-hash", "", nil, []string{source})
	got, err := downloads.GetByID(ctx, dl.ID)
	if err != nil || got.Status != models.StateImported {
		t.Fatalf("legacy qbit import status = %+v, %v", got, err)
	}
	rows, err := books.ListFiles(ctx, book.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("imported files = %+v, %v", rows, err)
	}
	linked, err := os.Stat(rows[0].Path)
	if err != nil || !os.SameFile(before, linked) {
		t.Fatalf("legacy download copied or moved its source: %v", err)
	}
	if after, err := os.Stat(source); err != nil || !os.SameFile(before, after) {
		t.Fatalf("legacy source moved: %v", err)
	}
}

func TestNonQbitLegacyImport_RetainsConfiguredCopyMode(t *testing.T) {
	library := t.TempDir()
	s, book, downloads, books, settings, _, ctx := sharedFormatFixture(t, library)
	if err := settings.Set(ctx, "import.mode", "copy"); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "We Who Wrestle with God.epub")
	if err := os.WriteFile(source, bookSized("other client"), 0o644); err != nil {
		t.Fatal(err)
	}
	dl := &models.Download{GUID: "legacy-transmission-copy", Title: book.Title, BookID: &book.ID, Status: models.StateCompleted, Protocol: "torrent"}
	if err := downloads.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	s.tryImportInternal(context.Background(), dl, source, "transmission", "test-hash", "", nil, []string{source})
	got, err := downloads.GetByID(ctx, dl.ID)
	if err != nil || got.Status != models.StateImported {
		t.Fatalf("non-qbit status = %+v, %v", got, err)
	}
	files, err := books.ListFiles(ctx, book.ID)
	if err != nil || len(files) != 1 {
		t.Fatalf("non-qbit library files = %+v, %v", files, err)
	}
	src, _ := os.Stat(source)
	dest, err := os.Stat(files[0].Path)
	if err != nil || os.SameFile(src, dest) {
		t.Fatalf("non-qbit configured copy became hardlink: %v", err)
	}
}
