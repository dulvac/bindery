package importer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

func TestVariantImport_RejectedSourcesStayRejectedAcrossQbitRetries(t *testing.T) {
	for _, audio := range []bool{false, true} {
		name := "ebook"
		format := models.MediaTypeEbook
		if audio {
			name = "audiobook"
			format = models.MediaTypeAudiobook
		}
		t.Run(name, func(t *testing.T) {
			f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", format, "eng")
			content := f.sources
			var file string
			if audio {
				content = filepath.Join(f.sources, "recording")
				if err := os.Mkdir(content, 0o755); err != nil {
					t.Fatal(err)
				}
				file = filepath.Join(content, "English catalogue label.m4b")
				if err := os.WriteFile(file, mp4File(atom("\xa9alb", dataAtom(1, []byte("English catalogue label"))), atom("\xa9ART", dataAtom(1, []byte("Sample Author")))), 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				file = variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
				content = file
			}
			const hash = "abcdef1234567890abcdef1234567890abcdef12"
			var mutators int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/auth/login":
					_, _ = w.Write([]byte("Ok."))
				case "/api/v2/torrents/info":
					_ = json.NewEncoder(w).Encode([]map[string]any{{"hash": hash, "name": "English catalogue label", "state": "stalledUP", "progress": 1.0, "size": 8192, "amount_left": 0, "save_path": f.sources, "content_path": content}})
				case "/api/v2/torrents/files":
					_ = json.NewEncoder(w).Encode([]map[string]any{{"name": filepath.ToSlash(strings.TrimPrefix(file, f.sources+string(filepath.Separator))), "size": 8192, "progress": 1.0}})
				default:
					mutators++
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer srv.Close()
			host, port := scannerTestHostPort(t, srv.URL)
			client := &models.DownloadClient{Name: "test-qbit", Type: "qbittorrent", Host: host, Port: port, Enabled: true, Category: "mam"}
			if err := db.NewDownloadClientRepo(f.database).Create(f.ctx, client); err != nil {
				t.Fatal(err)
			}
			dl := f.addDownload(t, "English catalogue label [ENG / "+map[bool]string{true: "M4B", false: "EPUB"}[audio]+"]")
			if _, err := f.database.ExecContext(f.ctx, `UPDATE downloads SET download_client_id=?, torrent_id=? WHERE id=?`, client.ID, hash, dl.ID); err != nil {
				t.Fatal(err)
			}
			rejection := file
			if audio {
				rejection = content
			}
			if err := f.rejections.Record(f.ctx, f.book.ID, f.variant.ID, dl.ID, rejection, "owner rejected this exact source"); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if attempt > 0 {
					ok, found, err := f.downloads.ResetImportRetry(f.ctx, dl.ID)
					if err != nil || !ok || !found {
						t.Fatalf("rearm retry = %t %t %v", ok, found, err)
					}
				}
				f.scanner.checkQbittorrentDownloads(f.ctx, client)
				f.assertLibraryFiles(t, 0)
				variants, err := f.books.VariantRepo().ListByBook(f.ctx, f.book.ID)
				if err != nil || len(variants) != 1 || variants[0].Status != models.VariantReview {
					t.Fatalf("previously rejected source must leave variant in review: %+v %v", variants, err)
				}
				got, err := f.downloads.GetByID(f.ctx, dl.ID)
				if err != nil || got.Status == models.StateImported {
					t.Fatalf("rejected download imported on attempt %d: %+v %v", attempt, got, err)
				}
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatalf("torrent source changed: %v", err)
			}
			if mutators != 0 {
				t.Fatalf("qBittorrent mutator/unexpected calls: %d", mutators)
			}
		})
	}
}

// A verified qBittorrent source imports without a client mutation.
func TestVariantImport_ValidQbitSourceStaysSeedingAfterImport(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	file := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	const hash = "abcdef1234567890abcdef1234567890abcdef12"
	var mutators int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"hash": hash, "name": "English catalogue label", "state": "stalledUP", "progress": 1.0, "size": 8192, "amount_left": 0, "save_path": f.sources, "content_path": file}})
		case "/api/v2/torrents/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"name": filepath.Base(file), "size": 8192, "progress": 1.0}})
		default:
			mutators++
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	host, port := scannerTestHostPort(t, srv.URL)
	client := &models.DownloadClient{Name: "test-qbit", Type: "qbittorrent", Host: host, Port: port, Enabled: true, Category: "mam"}
	if err := db.NewDownloadClientRepo(f.database).Create(f.ctx, client); err != nil {
		t.Fatal(err)
	}
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	if _, err := f.database.ExecContext(f.ctx, `UPDATE downloads SET download_client_id=?, torrent_id=? WHERE id=?`, client.ID, hash, dl.ID); err != nil {
		t.Fatal(err)
	}
	f.scanner.checkQbittorrentDownloads(f.ctx, client)
	f.assertVariantPlaced(t, dl)
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("qBittorrent source removed: %v", err)
	}
	if mutators != 0 {
		t.Fatalf("qBittorrent mutator/unexpected calls: %d", mutators)
	}
}

func TestVariantRetry_ReviewedVariantNeedsExactAdminOverrideOrWantedTransition(t *testing.T) {
	for _, approve := range []bool{false, true} {
		t.Run(map[bool]string{false: "no approval", true: "exact admin approval"}[approve], func(t *testing.T) {
			f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
			source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
			dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
			if err := f.books.VariantRepo().SetStatus(f.ctx, f.variant.ID, models.VariantReview); err != nil {
				t.Fatal(err)
			}
			if approve {
				payload, _ := json.Marshal(map[string]any{"actorUserId": int64(42), "variantId": f.variant.ID, "downloadId": dl.ID, "reason": "owner approved this retry"})
				if err := f.history.Create(f.ctx, &models.HistoryEvent{BookID: &f.book.ID, EventType: "variantLanguageOverride", Data: string(payload)}); err != nil {
					t.Fatal(err)
				}
			}
			f.importSource(t, dl, source)
			if approve {
				f.assertVariantPlaced(t, dl)
			} else {
				variantBlocked(t, f, dl, "in review")
				f.assertLibraryFiles(t, 0)
			}
		})
	}
}

func TestVariantRetry_ReviewedCollisionKeepsReviewUntilSuccessfulPlacement(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	if err := f.books.VariantRepo().SetStatus(f.ctx, f.variant.ID, models.VariantReview); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"actorUserId": int64(42), "variantId": f.variant.ID, "downloadId": dl.ID, "reason": "approved retry"})
	if err := f.history.Create(f.ctx, &models.HistoryEvent{BookID: &f.book.ID, EventType: "variantLanguageOverride", Data: string(payload)}); err != nil {
		t.Fatal(err)
	}
	dest := variantEbookTarget(t, f, source)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("other owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.importSource(t, dl, source)
	variantBlocked(t, f, dl, "no-replace")
	if rows, err := f.books.VariantRepo().ListByBook(f.ctx, f.book.ID); err != nil || len(rows) != 1 || rows[0].Status != models.VariantReview {
		t.Fatalf("failed placement cleared review: %+v %v", rows, err)
	}
	if rows, err := f.books.ListFiles(f.ctx, f.book.ID); err != nil || len(rows) != 0 {
		t.Fatalf("failed placement tracked file: %+v %v", rows, err)
	}
	if b, err := os.ReadFile(dest); err != nil || string(b) != "other owner" {
		t.Fatalf("collision replaced another owner: %q %v", b, err)
	}
}

func TestVariantRetry_WithdrawnAuditBeforeTrackingCannotClearReview(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	if err := f.books.VariantRepo().SetStatus(f.ctx, f.variant.ID, models.VariantReview); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"actorUserId": int64(42), "variantId": f.variant.ID, "downloadId": dl.ID, "reason": "approved retry"})
	audit := &models.HistoryEvent{BookID: &f.book.ID, EventType: "variantLanguageOverride", Data: string(payload)}
	if err := f.history.Create(f.ctx, audit); err != nil {
		t.Fatal(err)
	}
	variantBeforeTrackHook = func() {
		if err := f.history.Delete(f.ctx, audit.ID); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { variantBeforeTrackHook = nil })
	f.importSource(t, dl, source)
	variantBlocked(t, f, dl, "stranded")
	if rows, err := f.books.VariantRepo().ListByBook(f.ctx, f.book.ID); err != nil || len(rows) != 1 || rows[0].Status != models.VariantReview {
		t.Fatalf("withdrawn audit cleared review: %+v %v", rows, err)
	}
	if rows, err := f.books.ListFiles(f.ctx, f.book.ID); err != nil || len(rows) != 0 {
		t.Fatalf("withdrawn audit tracked file: %+v %v", rows, err)
	}
	if _, err := os.Lstat(variantEbookTarget(t, f, source)); err != nil {
		t.Fatalf("promoted link must remain for reconciliation: %v", err)
	}
}

func TestVariantRetry_TrackedLinkBeforeDownloadStatusIsIdempotent(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	f.importSource(t, dl, source)
	f.assertVariantPlaced(t, dl)
	first := variantImportedFiles(t, f, 1)[0]
	before, err := os.Lstat(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the book_file transaction but before the
	// download's terminal transition is durable.
	if _, err := f.database.ExecContext(f.ctx, `UPDATE downloads SET status='completed' WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	f.importSource(t, dl, source)
	variantImportedDownload(t, f, dl.ID)
	rows := variantImportedFiles(t, f, 1)
	after, err := os.Lstat(rows[0].Path)
	if err != nil || rows[0].ID != first.ID || !os.SameFile(before, after) {
		t.Fatalf("retry created or replaced tracked link: %+v %v", rows, err)
	}
}

func TestVariantRetry_TrackedAudioFolderBeforeDownloadStatusIsIdempotent(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
	folder, files := variantAudioSources(t, f)
	dl := f.addDownload(t, "Dune by Frank Herbert [ENG / M4B]")
	f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "hash", "", nil, files)
	f.assertVariantPlaced(t, dl)
	first := variantImportedFiles(t, f, 1)[0]
	before, err := os.Lstat(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.ExecContext(f.ctx, `UPDATE downloads SET status='completed' WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "hash", "", nil, files)
	variantImportedDownload(t, f, dl.ID)
	rows := variantImportedFiles(t, f, 1)
	after, err := os.Lstat(rows[0].Path)
	if err != nil || rows[0].ID != first.ID || !os.SameFile(before, after) {
		t.Fatalf("retry created or replaced audio folder: %+v %v", rows, err)
	}
}

func TestVariantScan_RejectsInodeMatchButReconcilesUnrelatedBook(t *testing.T) {
	for _, audio := range []bool{false, true} {
		name := "ebook"
		format := models.MediaTypeEbook
		if audio {
			name = "audiobook"
			format = models.MediaTypeAudiobook
		}
		t.Run(name, func(t *testing.T) {
			f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", format, "eng")
			folder := filepath.Join(f.sources, "rejected audio")
			file := variantEPUBSource(t, f.sources, "rejected.epub", "english.epub")
			rejectedSource := file
			if audio {
				if err := os.Mkdir(folder, 0o755); err != nil {
					t.Fatal(err)
				}
				file = filepath.Join(folder, "English catalogue label.m4b")
				if err := os.WriteFile(file, mp4File(atom("\xa9alb", dataAtom(1, []byte("English catalogue label"))), atom("\xa9ART", dataAtom(1, []byte("Sample Author")))), 0o600); err != nil {
					t.Fatal(err)
				}
				rejectedSource = folder
			}
			dl := f.addDownload(t, "English catalogue label")
			if err := f.rejections.Record(f.ctx, f.book.ID, f.variant.ID, dl.ID, rejectedSource, "wrong edition"); err != nil {
				t.Fatal(err)
			}
			libFolder := filepath.Join(f.library, "Sample Author", "English catalogue label")
			if err := os.MkdirAll(libFolder, 0o755); err != nil {
				t.Fatal(err)
			}
			libCopy := filepath.Join(libFolder, "English catalogue label"+filepath.Ext(file))
			if err := os.Link(file, libCopy); err != nil {
				t.Fatal(err)
			}
			// The library path is NOT the rejected path; only the inode reveals it.
			if libCopy == rejectedSource {
				t.Fatal("fixture did not separate source from library")
			}
			f.scanner.ScanLibrary(f.ctx)
			rows, err := f.books.ListFiles(f.ctx, f.book.ID)
			if err != nil || len(rows) != 0 {
				t.Fatalf("rescan reattached rejected inode: %+v %v", rows, err)
			}
			// A different inode in the same library remains eligible.
			other := &models.Book{ForeignID: "other-reconcile", AuthorID: f.book.AuthorID, Title: "Other Work", SortTitle: "Other Work", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted, Monitored: true, AnyEditionOK: true, MetadataProvider: "openlibrary"}
			if err := f.books.Create(f.ctx, other); err != nil {
				t.Fatal(err)
			}
			otherPath := filepath.Join(f.library, "Sample Author", "Other Work.epub")
			if err := os.WriteFile(otherPath, bookSized("this is another book"), 0o600); err != nil {
				t.Fatal(err)
			}
			f.scanner.ScanLibrary(f.ctx)
			rows, err = f.books.ListFiles(f.ctx, f.book.ID)
			if err != nil || len(rows) != 0 {
				t.Fatalf("rejected inode reattached after second scan: %+v %v", rows, err)
			}
			otherFiles, err := f.books.ListFiles(f.ctx, other.ID)
			if err != nil || len(otherFiles) != 1 {
				t.Fatalf("unrelated book not reconciled: %+v %v", otherFiles, err)
			}
		})
	}
}

func TestVariantScan_ChangedSourceParentSymlinkFailsClosed(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	folder := filepath.Join(f.sources, "content")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	source := variantEPUBSource(t, folder, "English catalogue label.epub", "english.epub")
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	if err := f.rejections.Record(f.ctx, f.book.ID, f.variant.ID, dl.ID, source, "rejected file"); err != nil {
		t.Fatal(err)
	}
	libFolder := filepath.Join(f.library, "Sample Author")
	if err := os.MkdirAll(libFolder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, filepath.Join(libFolder, "English catalogue label.epub")); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(f.sources, "original")
	if err := os.Rename(folder, original); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	_ = variantEPUBSource(t, other, "English catalogue label.epub", "english.epub")
	if err := os.Symlink(other, folder); err != nil {
		t.Skipf("symlink not available: %v", err)
	}
	f.scanner.ScanLibrary(f.ctx)
	rows, err := f.books.ListFiles(f.ctx, f.book.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("symlinked rejected source reattached as %v: %v", rows, err)
	}
}

func TestVariantScan_RejectedSeriesInodeRemainsVisibleAsUnmatched(t *testing.T) {
	f := newVariantReviewFixture(t, "Quantum Gardens", "Ursula North", models.MediaTypeEbook, "eng")
	series := db.NewSeriesRepo(f.database)
	ser := &models.Series{ForeignID: "manual:stormlight", Title: "Stormlight Archive"}
	if err := series.Create(f.ctx, ser); err != nil {
		t.Fatal(err)
	}
	if err := series.LinkBook(f.ctx, ser.ID, f.book.ID, "1", true); err != nil {
		t.Fatal(err)
	}
	f.scanner.WithSeriesRepo(series).WithSettings(db.NewSettingsRepo(f.database))
	source := filepath.Join(f.sources, "source.epub")
	if err := os.WriteFile(source, bookSized("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dl := f.addDownload(t, "bad series source")
	if err := f.rejections.Record(f.ctx, f.book.ID, f.variant.ID, dl.ID, source, "not the requested book"); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(f.library, "[Stormlight Archive, Book 1] The Way of Kings - Brandon Sanderson.epub")
	if err := os.Link(source, linked); err != nil {
		t.Fatal(err)
	}
	f.scanner.ScanLibrary(f.ctx)
	files, err := f.books.ListFiles(f.ctx, f.book.ID)
	if err != nil || len(files) != 0 {
		t.Fatalf("series tier attached rejected inode: %+v %v", files, err)
	}
	setting, err := db.NewSettingsRepo(f.database).Get(f.ctx, "library.lastScan")
	if err != nil || setting == nil {
		t.Fatalf("scan result missing: %+v %v", setting, err)
	}
	var result struct {
		Unmatched int `json:"unmatched"`
	}
	if err := json.Unmarshal([]byte(setting.Value), &result); err != nil {
		t.Fatal(err)
	}
	if result.Unmatched != 1 {
		t.Fatalf("rejected series source disappeared from scan: %+v", result)
	}
}

func TestVariantImportAndScan_RejectionReadErrorFailsClosed(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	// Keep all other DB tables live; only the rejection lookup is broken.
	if _, err := f.database.ExecContext(f.ctx, "DROP TABLE variant_rejections"); err != nil {
		t.Fatal(err)
	}
	f.importSource(t, dl, source)
	f.assertLibraryFiles(t, 0)
	libFolder := filepath.Join(f.library, "Sample Author")
	if err := os.MkdirAll(libFolder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, filepath.Join(libFolder, "English catalogue label.epub")); err != nil {
		t.Fatal(err)
	}
	f.scanner.ScanLibrary(f.ctx)
	rows, err := f.books.ListFiles(f.ctx, f.book.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("scan attached source after rejection DB failure: %+v %v", rows, err)
	}
}

func TestVariantImport_RejectionWriteFailureLeavesNoLibraryLink(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "romanian-en-opf.epub")
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	if _, err := f.database.ExecContext(f.ctx, `CREATE TRIGGER refuse_rejection BEFORE INSERT ON variant_rejections BEGIN SELECT RAISE(FAIL, 'test rejection write failed'); END`); err != nil {
		t.Fatal(err)
	}
	f.importSource(t, dl, source)
	f.assertLibraryFiles(t, 0)
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source changed on DB failure: %v", err)
	}
	got, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || got.Status == models.StateImported {
		t.Fatalf("download marked imported after rejection write failure: %+v %v", got, err)
	}
}

func TestVariantImport_MixedFormatsSelectsEbookWithoutAudioLink(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	epub := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	other := filepath.Join(f.sources, "bonus.m4b")
	if err := os.WriteFile(other, mp4File(atom("\xa9alb", dataAtom(1, []byte("English catalogue label")))), 0o600); err != nil {
		t.Fatal(err)
	}
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	author, err := db.NewAuthorRepo(f.database).GetByID(f.ctx, f.book.AuthorID)
	if err != nil {
		t.Fatal(err)
	}
	selected, reviewed, reason, err := f.scanner.reviewVariantImport(f.ctx, dl, f.book, author, models.MediaTypeEbook, f.sources, []string{epub, other})
	if err != nil || reviewed || len(selected) != 1 || selected[0].path != epub {
		t.Fatalf("ebook selection included audiobook: %+v reviewed=%t reason=%q err=%v", selected, reviewed, reason, err)
	}
	f.scanner.tryImportInternal(f.ctx, dl, f.sources, "qbittorrent", "test-hash", models.MediaTypeEbook, nil, []string{epub, other})
	f.assertVariantPlaced(t, dl)
	for _, source := range []string{epub, other} {
		if _, err := os.Stat(source); err != nil {
			t.Fatalf("torrent source changed: %s: %v", source, err)
		}
	}
}

func TestVariantImport_ManualFormatHintCannotTurnAudioIntoEbookVariant(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	audio := filepath.Join(f.sources, "English catalogue label.m4b")
	if err := os.WriteFile(audio, mp4File(atom("\xa9alb", dataAtom(1, []byte("English catalogue label")))), 0o600); err != nil {
		t.Fatal(err)
	}
	dl := f.addDownload(t, "English catalogue label [ENG / M4B]")
	f.scanner.tryImportInternal(f.ctx, dl, audio, "qbittorrent", "test-hash", models.MediaTypeEbook, nil, []string{audio})
	f.assertLibraryFiles(t, 0)
}

func TestVariantImport_MissingTorrentFileListCannotWalkSharedRoot(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	_ = variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	f.scanner.tryImportInternal(f.ctx, dl, f.sources, "qbittorrent", "test-hash", "", nil, nil)
	f.assertLibraryFiles(t, 0)
	got, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || got.Status == models.StateImported {
		t.Fatalf("variant import walked unowned torrent root: %+v %v", got, err)
	}
}

func TestVariantImport_CannotOmitPersistedVariantWithStaleDownloadObject(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "romanian-en-opf.epub")
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	stale := *dl
	stale.VariantID = nil // a caller must not turn a variant import into legacy behavior
	f.importSource(t, &stale, source)
	f.assertReview(t, source)
}

func TestVariantImport_DoesNotOverwriteExistingLibraryPath(t *testing.T) {
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
	sentinel := []byte("another owner's existing library entry")
	if err := os.WriteFile(dest, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	f.importSource(t, dl, source)
	content, err := os.ReadFile(dest)
	if err != nil || string(content) != string(sentinel) {
		t.Fatalf("existing legacy library file overwritten (length %d): %v", len(content), err)
	}
	variantImportedDownload(t, f, dl.ID)
	rows := variantImportedFiles(t, f, 1)
	if rows[0].Path == dest || !strings.Contains(rows[0].Path, " [eng].epub") {
		t.Fatalf("variant took legacy filename: %+v", rows[0])
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("torrent source changed: %v", err)
	}
}

func TestVariantImport_MissingSourceCannotUseBookLevelShortcut(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
	prior := filepath.Join(f.library, "already-imported")
	if err := os.Mkdir(prior, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.books.AddBookFile(f.ctx, f.book.ID, models.MediaTypeAudiobook, prior); err != nil {
		t.Fatal(err)
	}
	dl := f.addDownload(t, "Dune by Frank Herbert [ENG / M4B]")
	f.scanner.tryImportInternal(f.ctx, dl, filepath.Join(f.sources, "gone"), "qbittorrent", "test-hash", "", nil, nil)
	got, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || got.Status == models.StateImported {
		t.Fatalf("unreviewed source completed through shortcut: %+v %v", got, err)
	}
}

func TestVariantImport_RejectsSymlinkAboveReportedContentPath(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	outside := t.TempDir()
	file := variantEPUBSource(t, outside, "English catalogue label.epub", "english.epub")
	link := filepath.Join(f.sources, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink not available: %v", err)
	}
	reported := filepath.Join(link, filepath.Base(file))
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	// The client reports the file itself as content_path; checking only the
	// component at or below it misses its linked parent directory.
	f.scanner.tryImportInternal(f.ctx, dl, reported, "qbittorrent", "test-hash", "", nil, []string{reported})
	f.assertLibraryFiles(t, 0)
}

func TestVariantImport_RejectsSymlinkedSourceParent(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	outside := t.TempDir()
	file := variantEPUBSource(t, outside, "English catalogue label.epub", "english.epub")
	link := filepath.Join(f.sources, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink not available: %v", err)
	}
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	f.scanner.tryImportInternal(context.Background(), dl, f.sources, "qbittorrent", "test-hash", "", nil, []string{filepath.Join(link, filepath.Base(file))})
	f.assertLibraryFiles(t, 0)
}
