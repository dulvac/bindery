package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/downloader/qbittorrent"
	"github.com/vavallee/bindery/internal/models"
)

type rejectSourceFixture struct {
	conn     *sql.DB
	database *db.BookRepo
	router   http.Handler
	book     *models.Book
	variant  models.BookVariant
	download *models.Download
	file     models.BookFile
	source   string
	library  string
	root     string
	ctx      context.Context
	rejected *db.VariantRejectionRepo
	qbit     *fakeRejectionQbit
	client   *models.DownloadClient
}

type fakeRejectionQbit struct {
	torrents []qbittorrent.Torrent
	files    []qbittorrent.File
	err      error
}

func (q *fakeRejectionQbit) GetTorrents(_ context.Context, category string) ([]qbittorrent.Torrent, error) {
	if category != "" {
		return nil, fmt.Errorf("rejection must not filter torrents by category")
	}
	return q.torrents, q.err
}

func (q *fakeRejectionQbit) Files(_ context.Context, hash string) ([]qbittorrent.File, error) {
	if len(q.torrents) == 0 || !strings.EqualFold(hash, q.torrents[0].Hash) {
		return nil, fmt.Errorf("files requested for unrelated torrent")
	}
	return q.files, q.err
}

func newRejectSourceFixture(t *testing.T) *rejectSourceFixture {
	t.Helper()
	conn, err := db.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "downloads")
	source := filepath.Join(root, "mam", "release", "dune.epub")
	library := filepath.Join(base, "library", "dune.epub")
	for _, path := range []string{source, library} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(source, []byte("source torrent bytes remain unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, library); err != nil {
		t.Skipf("hardlinks unavailable on test volume: %v", err)
	}
	books := db.NewBookRepo(conn)
	author := &models.Author{ForeignID: "reject-a", Name: "Frank Herbert", SortName: "Herbert, Frank"}
	if err := db.NewAuthorRepo(conn).Create(ctx, author); err != nil {
		t.Fatal(err)
	}
	book := &models.Book{ForeignID: "reject-b", AuthorID: author.ID, Title: "Dune", SortTitle: "Dune", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := books.Create(ctx, book); err != nil {
		t.Fatal(err)
	}
	variants, err := books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"rum", "eng"})
	if err != nil {
		t.Fatal(err)
	}
	client := &models.DownloadClient{Name: "test-only", Type: "qbittorrent", Host: "fake.invalid", Port: 80, Username: "fake", Password: "fake", PathRemap: "/qbit/data:" + filepath.Join(root, "mam")}
	if err := db.NewDownloadClientRepo(conn).Create(ctx, client); err != nil {
		t.Fatal(err)
	}
	hash := "1234567890abcdef1234567890abcdef12345678"
	dl := &models.Download{GUID: "dune-romanian", BookID: &book.ID, Title: "Dune by Frank Herbert [ROM / EPUB]", Status: models.StateImported, Protocol: "torrent", DownloadClientID: &client.ID, TorrentID: &hash}
	if err := db.NewDownloadRepo(conn).Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE downloads SET completed_at = ? WHERE id = ?`, time.Now().UTC(), dl.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.NewDownloadRepo(conn).SetImportPath(ctx, dl.ID, filepath.Dir(source)); err != nil {
		t.Fatal(err)
	}
	if err := books.AddBookFile(ctx, book.ID, models.MediaTypeEbook, library); err != nil {
		t.Fatal(err)
	}
	files, err := books.ListBookFiles(ctx, book.ID)
	if err != nil || len(files) != 1 {
		t.Fatalf("tracked file: %+v %v", files, err)
	}
	rejected := db.NewVariantRejectionRepo(conn)
	qbit := &fakeRejectionQbit{
		torrents: []qbittorrent.Torrent{{Hash: hash, SavePath: "/qbit/data", ContentPath: "/qbit/data/release"}},
		files:    []qbittorrent.File{{Name: "release/dune.epub", Size: int64(len("source torrent bytes remain unchanged"))}},
	}
	f := &rejectSourceFixture{conn: conn, database: books, book: book, variant: variants[1], download: dl,
		file: files[0], source: source, library: library, root: root, ctx: ctx, rejected: rejected, qbit: qbit, client: client}
	f.route()
	return f
}

func (f *rejectSourceFixture) route() { f.routeWithOptions("", filepath.Dir(f.library)) }

func (f *rejectSourceFixture) routeWithRemap(remap string) {
	f.routeWithOptions(remap, filepath.Dir(f.library))
}

func (f *rejectSourceFixture) routeWithLibraryRoots(roots ...string) {
	f.routeWithOptions("", roots...)
}

func (f *rejectSourceFixture) routeWithOptions(remap string, libraryRoots ...string) {
	if len(libraryRoots) == 0 {
		libraryRoots = []string{filepath.Dir(f.library)}
	}
	h := NewBookHandler(f.database, nil, nil, nil).
		WithDownloads(db.NewDownloadRepo(f.conn)).
		WithRoots(NewLibraryRoots(nil, libraryRoots...)).
		WithVariantRejections(f.rejected, f.root).
		WithRejectionTorrentSource(db.NewDownloadClientRepo(f.conn), remap)
	h.rejectionTorrentReader = func(*models.DownloadClient) rejectionTorrentReader { return f.qbit }
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) { RegisterVariantRejectionRoute(r, h) })
	f.router = r
}

func (f *rejectSourceFixture) post(t *testing.T, bookID, variantID int64, body map[string]any, role string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("/api/v1/book/%d/variants/%d/reject-source", bookID, variantID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	req = req.WithContext(auth.WithUserRole(req.Context(), role))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *rejectSourceFixture) body() map[string]any {
	return map[string]any{"downloadId": f.download.ID, "bookFileId": f.file.ID, "reason": "wrong language"}
}

func (f *rejectSourceFixture) assertNoRejection(t *testing.T) {
	t.Helper()
	got, err := f.rejected.IsRejected(f.ctx, f.book.ID, f.variant.ID, f.source)
	if err != nil || got {
		t.Fatalf("unsafe rejection recorded: %t %v", got, err)
	}
}

// A valid call only records the exact source identity. Neither the library
// link, the torrent bytes nor the download's lifecycle is changed.
func TestRejectSourceAdminHardlinkIdempotentAndNonDestructive(t *testing.T) {
	f := newRejectSourceFixture(t)
	for i := 0; i < 2; i++ {
		if rec := f.post(t, f.book.ID, f.variant.ID, f.body(), auth.RoleAdmin); rec.Code != http.StatusOK {
			t.Fatalf("call %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	got, err := f.rejected.IsRejected(f.ctx, f.book.ID, f.variant.ID, f.source)
	if err != nil || !got {
		t.Fatalf("expected source rejection: %t %v", got, err)
	}
	files, err := f.database.ListBookFiles(f.ctx, f.book.ID)
	if err != nil || len(files) != 1 || files[0].ID != f.file.ID {
		t.Fatalf("library association changed: %+v %v", files, err)
	}
	for _, p := range []string{f.source, f.library} {
		b, err := os.ReadFile(p)
		if err != nil || string(b) != "source torrent bytes remain unchanged" {
			t.Errorf("file at %s changed: %q %v", p, b, err)
		}
	}
	download, err := db.NewDownloadRepo(f.conn).GetByID(f.ctx, f.download.ID)
	if err != nil || download == nil || download.Status != models.StateImported {
		t.Fatalf("download changed: %+v %v", download, err)
	}
	variants, err := f.database.VariantRepo().ListByBook(f.ctx, f.book.ID)
	if err != nil || len(variants) != 2 || variants[0].Status != models.VariantWanted {
		t.Fatalf("unrelated English variant changed: %+v %v", variants, err)
	}
	if rejected, err := f.rejected.IsRejected(f.ctx, f.book.ID, variants[0].ID, f.source); err != nil || rejected {
		t.Fatalf("admin rejection leaked to English variant: %t %v", rejected, err)
	}
}

func TestRejectSourceRequiresAdmin(t *testing.T) {
	f := newRejectSourceFixture(t)
	for _, role := range []string{"", auth.RoleUser, auth.RoleRequester} {
		if rec := f.post(t, f.book.ID, f.variant.ID, f.body(), role); rec.Code != http.StatusForbidden {
			t.Errorf("role %q = %d: %s; want 403", role, rec.Code, rec.Body.String())
		}
	}
	f.assertNoRejection(t)
}

func TestRejectSourceRejectsWrongOwnership(t *testing.T) {
	f := newRejectSourceFixture(t)
	other := &models.Book{ForeignID: "reject-other", AuthorID: f.book.AuthorID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := f.database.Create(f.ctx, other); err != nil {
		t.Fatal(err)
	}
	otherVariants, err := f.database.VariantRepo().EnsureWanted(f.ctx, other.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	otherDL := &models.Download{GUID: "other-rejection-dl", BookID: &other.ID, Title: "Other", Status: models.StateImported}
	if err := db.NewDownloadRepo(f.conn).Create(f.ctx, otherDL); err != nil {
		t.Fatal(err)
	}
	otherLibrary := filepath.Join(filepath.Dir(f.library), "other.epub")
	if err := os.WriteFile(otherLibrary, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.database.AddBookFile(f.ctx, other.ID, models.MediaTypeEbook, otherLibrary); err != nil {
		t.Fatal(err)
	}
	otherFiles, err := f.database.ListBookFiles(f.ctx, other.ID)
	if err != nil || len(otherFiles) != 1 {
		t.Fatalf("other tracked file: %+v %v", otherFiles, err)
	}
	cases := []struct {
		name          string
		book, variant int64
		body          map[string]any
	}{
		{"missing book", 99999, f.variant.ID, f.body()},
		{"foreign variant", f.book.ID, otherVariants[0].ID, f.body()},
		{"foreign download", f.book.ID, f.variant.ID, map[string]any{"downloadId": otherDL.ID, "bookFileId": f.file.ID, "reason": "wrong"}},
		{"foreign file", f.book.ID, f.variant.ID, map[string]any{"downloadId": f.download.ID, "bookFileId": otherFiles[0].ID, "reason": "wrong"}},
		{"other book route", other.ID, f.variant.ID, f.body()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.post(t, tc.book, tc.variant, tc.body, auth.RoleAdmin)
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusConflict {
				t.Fatalf("%d %s; want 404/409", rec.Code, rec.Body.String())
			}
			f.assertNoRejection(t)
		})
	}
}

func TestRejectSourceRefusesMissingOrUnprovenProvenance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *rejectSourceFixture)
	}{
		{"missing torrent hash", func(t *testing.T, f *rejectSourceFixture) {
			if _, err := f.conn.ExecContext(f.ctx, `UPDATE downloads SET torrent_id = NULL WHERE id = ?`, f.download.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing client credentials", func(t *testing.T, f *rejectSourceFixture) {
			if _, err := f.conn.ExecContext(f.ctx, `UPDATE download_clients SET password = '' WHERE id = ?`, f.client.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty torrent file list", func(_ *testing.T, f *rejectSourceFixture) {
			f.qbit.files = nil
		}},
		{"unknown torrent hash", func(_ *testing.T, f *rejectSourceFixture) {
			f.qbit.torrents = nil
		}},
		{"bad remote file name", func(_ *testing.T, f *rejectSourceFixture) {
			f.qbit.files[0].Name = "../foreign.epub"
		}},
		{"remote source symlink", func(t *testing.T, f *rejectSourceFixture) {
			if err := os.Remove(f.source); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.library, f.source); err != nil {
				t.Fatal(err)
			}
		}},
		{"source under root different inode", func(t *testing.T, f *rejectSourceFixture) {
			if err := os.Remove(f.source); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.source, []byte("impostor"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"source outside root", func(_ *testing.T, f *rejectSourceFixture) {
			f.qbit.torrents[0].SavePath = filepath.Dir(f.library)
			f.qbit.torrents[0].ContentPath = filepath.Dir(f.library)
			f.qbit.files[0].Name = filepath.Base(f.library)
		}},
		{"symlink library", func(t *testing.T, f *rejectSourceFixture) {
			if err := os.Remove(f.library); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.source, f.library); err != nil {
				t.Fatal(err)
			}
		}},
		{"two matching hardlinks in release", func(t *testing.T, f *rejectSourceFixture) {
			if err := os.Link(f.source, filepath.Join(filepath.Dir(f.source), "duplicate.epub")); err != nil {
				t.Fatal(err)
			}
		}},
		{"uncompleted download", func(t *testing.T, f *rejectSourceFixture) {
			if _, err := f.conn.ExecContext(f.ctx, `UPDATE downloads SET completed_at = NULL WHERE id = ?`, f.download.ID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRejectSourceFixture(t)
			tc.change(t, f)
			rec := f.post(t, f.book.ID, f.variant.ID, f.body(), auth.RoleAdmin)
			if rec.Code != http.StatusConflict {
				t.Fatalf("%d %s; want 409", rec.Code, rec.Body.String())
			}
			f.assertNoRejection(t)
		})
	}
}

// A shared save root is safe only when the identified torrent lists the
// exact file. The save root itself is never recorded as the source identity.
func TestRejectSourceSharedSaveRootUsesExactListedFile(t *testing.T) {
	f := newRejectSourceFixture(t)
	f.qbit.torrents[0].SavePath = f.root
	f.qbit.torrents[0].ContentPath = f.root
	f.qbit.files[0].Name = "mam/release/dune.epub"
	rec := f.post(t, f.book.ID, f.variant.ID, f.body(), auth.RoleAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("shared save root = %d %s; want 200", rec.Code, rec.Body.String())
	}
	if rejected, err := f.rejected.IsRejected(f.ctx, f.book.ID, f.variant.ID, f.source); err != nil || !rejected {
		t.Fatalf("exact source was not recorded: %t %v", rejected, err)
	}
}

func TestRejectSourceRefusesLibraryAsTorrentSource(t *testing.T) {
	f := newRejectSourceFixture(t)
	f.qbit.torrents[0].SavePath = filepath.Dir(f.library)
	f.qbit.torrents[0].ContentPath = f.library
	f.qbit.files[0].Name = filepath.Base(f.library)
	f.root = filepath.Dir(f.library) // deliberate overlapping roots
	f.route()
	rec := f.post(t, f.book.ID, f.variant.ID, f.body(), auth.RoleAdmin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("self-match = %d %s; want 409", rec.Code, rec.Body.String())
	}
	f.assertNoRejection(t)
}

func TestRejectSourceRefusesWrongVariantClaim(t *testing.T) {
	for _, update := range []struct {
		name string
		set  func(*testing.T, *rejectSourceFixture, int64)
	}{
		{"download", func(t *testing.T, f *rejectSourceFixture, english int64) {
			if _, err := f.conn.ExecContext(f.ctx, `UPDATE downloads SET variant_id = ? WHERE id = ?`, english, f.download.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"file", func(t *testing.T, f *rejectSourceFixture, english int64) {
			if _, err := f.database.UntrackFilePath(f.ctx, f.library); err != nil {
				t.Fatal(err)
			}
			files := db.NewBookFileRepo(f.conn)
			if err := files.TrackVariant(f.ctx, f.book.ID, english, models.MediaTypeEbook, f.library, "", "unverified"); err != nil {
				t.Fatal(err)
			}
			file, err := files.GetByPath(f.ctx, f.library)
			if err != nil || file == nil {
				t.Fatalf("English file claim: %#v, %v", file, err)
			}
			f.file = *file
		}},
	} {
		t.Run(update.name, func(t *testing.T) {
			f := newRejectSourceFixture(t)
			all, err := f.database.VariantRepo().ListByBook(f.ctx, f.book.ID)
			if err != nil {
				t.Fatal(err)
			}
			update.set(t, f, all[0].ID) // English, not the Romanian target.
			rec := f.post(t, f.book.ID, f.variant.ID, f.body(), auth.RoleAdmin)
			if rec.Code != http.StatusConflict {
				t.Fatalf("%d %s; want 409", rec.Code, rec.Body.String())
			}
			f.assertNoRejection(t)
		})
	}
}

func TestRejectSourceAcceptsSingleFileTorrentContentPath(t *testing.T) {
	f := newRejectSourceFixture(t)
	f.qbit.torrents[0].ContentPath = "/qbit/data/release/dune.epub"
	if err := db.NewDownloadRepo(f.conn).SetImportPath(f.ctx, f.download.ID, f.source); err != nil {
		t.Fatal(err)
	}
	rec := f.post(t, f.book.ID, f.variant.ID, f.body(), auth.RoleAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("single-file source = %d %s", rec.Code, rec.Body.String())
	}
	if rejected, err := f.rejected.IsRejected(f.ctx, f.book.ID, f.variant.ID, f.source); err != nil || !rejected {
		t.Fatalf("single-file source not recorded: %t %v", rejected, err)
	}
}

func TestRejectSourceMapsGlobalRemapWhenClientHasNone(t *testing.T) {
	f := newRejectSourceFixture(t)
	if _, err := f.conn.ExecContext(f.ctx, `UPDATE download_clients SET path_remap = '' WHERE id = ?`, f.client.ID); err != nil {
		t.Fatal(err)
	}
	f.routeWithRemap("/qbit/data:" + filepath.Join(f.root, "mam"))
	rec := f.post(t, f.book.ID, f.variant.ID, f.body(), auth.RoleAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("global path remap: %d %s; want 200", rec.Code, rec.Body.String())
	}
	if rejected, err := f.rejected.IsRejected(f.ctx, f.book.ID, f.variant.ID, f.source); err != nil || !rejected {
		t.Fatalf("remapped torrent source not recorded: %t %v", rejected, err)
	}
}

// A successful historical import did not persist ImportPath. The stored
// torrent hash and client id must suffice when the live torrent lists the
// exact hard-linked file. This fails on the old ImportPath-only proof.
func TestRejectSourceLegacyEbookWithoutImportPath(t *testing.T) {
	f := newRejectSourceFixture(t)
	if err := db.NewDownloadRepo(f.conn).SetImportPath(f.ctx, f.download.ID, ""); err != nil {
		t.Fatal(err)
	}
	rec := f.post(t, f.book.ID, f.variant.ID, f.body(), auth.RoleAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("historical EPUB: %d %s; want 200", rec.Code, rec.Body.String())
	}
	if rejected, err := f.rejected.IsRejected(f.ctx, f.book.ID, f.variant.ID, f.source); err != nil || !rejected {
		t.Fatalf("historical EPUB source not rejected: %t %v", rejected, err)
	}
}

// A book_files audiobook path is a directory. Every media member must prove
// the torrent's listed source inode, not the directory inode.
func newRejectSourceAudioFixture(t *testing.T) (*rejectSourceFixture, int64, int64, string, string) {
	t.Helper()
	f := newRejectSourceFixture(t)
	audioVariants, err := f.database.VariantRepo().EnsureWanted(f.ctx, f.book.ID, models.MediaTypeAudiobook, []string{"rum"})
	if err != nil || len(audioVariants) != 1 {
		t.Fatalf("audio variant: %+v %v", audioVariants, err)
	}
	if _, err := f.conn.ExecContext(f.ctx, `UPDATE downloads SET variant_id = ?, import_path = '' WHERE id = ?`, audioVariants[0].ID, f.download.ID); err != nil {
		t.Fatal(err)
	}
	f.qbit.torrents[0].ContentPath = "/qbit/data/recording"
	f.qbit.files = []qbittorrent.File{{Name: "recording/01.m4b"}, {Name: "recording/disc2/02.mp3"}}
	sourceDir := filepath.Join(f.root, "mam", "recording")
	libraryDir := filepath.Join(filepath.Dir(f.library), "recording")
	for _, name := range []string{"01.m4b", filepath.Join("disc2", "02.mp3")} {
		source := filepath.Join(sourceDir, name)
		library := filepath.Join(libraryDir, name)
		for _, dir := range []string{filepath.Dir(source), filepath.Dir(library)} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(source, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(source, library); err != nil {
			t.Skipf("hardlinks unavailable on test volume: %v", err)
		}
	}
	if err := f.database.AddBookFile(f.ctx, f.book.ID, models.MediaTypeAudiobook, libraryDir); err != nil {
		t.Fatal(err)
	}
	files, err := f.database.ListBookFiles(f.ctx, f.book.ID)
	if err != nil {
		t.Fatal(err)
	}
	var audioFile models.BookFile
	for _, file := range files {
		if file.Path == libraryDir {
			audioFile = file
		}
	}
	if audioFile.ID == 0 {
		t.Fatalf("audiobook folder not tracked: %+v", files)
	}
	return f, audioVariants[0].ID, audioFile.ID, sourceDir, libraryDir
}

func newRejectSourceAudioOverlapFixture(t *testing.T) (*rejectSourceFixture, int64, int64, string, string) {
	t.Helper()
	f := newRejectSourceFixture(t)
	audioVariants, err := f.database.VariantRepo().EnsureWanted(f.ctx, f.book.ID, models.MediaTypeAudiobook, []string{"rum"})
	if err != nil || len(audioVariants) != 1 {
		t.Fatalf("audio variant: %+v %v", audioVariants, err)
	}
	if _, err := f.conn.ExecContext(f.ctx, `UPDATE downloads SET variant_id = ?, import_path = '' WHERE id = ?`, audioVariants[0].ID, f.download.ID); err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join(f.root, "mam", "recording")
	sourceTrack := filepath.Join(sourceDir, "01.m4b")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceTrack, []byte("source audiobook"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.database.AddBookFile(f.ctx, f.book.ID, models.MediaTypeAudiobook, sourceDir); err != nil {
		t.Fatal(err)
	}
	files, err := f.database.ListBookFiles(f.ctx, f.book.ID)
	if err != nil {
		t.Fatal(err)
	}
	var audioFile models.BookFile
	for _, file := range files {
		if file.Path == sourceDir {
			audioFile = file
		}
	}
	if audioFile.ID == 0 {
		t.Fatalf("audiobook overlap folder not tracked: %+v", files)
	}
	f.qbit.torrents[0].ContentPath = "/qbit/data/recording/01.m4b"
	f.qbit.files = []qbittorrent.File{{Name: "recording/01.m4b", Size: int64(len("source audiobook"))}}
	f.routeWithLibraryRoots(f.root)
	return f, audioVariants[0].ID, audioFile.ID, sourceDir, sourceTrack
}

func TestRejectSourceAudiobookTrackedFolder(t *testing.T) {
	f, variantID, audioFileID, sourceDir, _ := newRejectSourceAudioFixture(t)
	body := map[string]any{"downloadId": f.download.ID, "bookFileId": audioFileID, "reason": "wrong language"}
	rec := f.post(t, f.book.ID, variantID, body, auth.RoleAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("audiobook folder: %d %s; want 200", rec.Code, rec.Body.String())
	}
	if rejected, err := f.rejected.IsRejected(f.ctx, f.book.ID, variantID, sourceDir); err != nil || !rejected {
		t.Fatalf("audiobook source folder not rejected: %t %v", rejected, err)
	}
}

func TestRejectSourceAudiobookRefusesSourceInsideTrackedFolder(t *testing.T) {
	t.Run("single-file torrent source", func(t *testing.T) {
		f, variantID, audioFileID, _, sourceTrack := newRejectSourceAudioOverlapFixture(t)
		body := map[string]any{"downloadId": f.download.ID, "bookFileId": audioFileID, "reason": "wrong language"}
		rec := f.post(t, f.book.ID, variantID, body, auth.RoleAdmin)
		if rec.Code != http.StatusConflict {
			t.Fatalf("single-file overlap: %d %s; want 409", rec.Code, rec.Body.String())
		}
		if rejected, err := f.rejected.IsRejected(f.ctx, f.book.ID, variantID, sourceTrack); err != nil || rejected {
			t.Fatalf("single-file overlap was rejected: %t %v", rejected, err)
		}
	})

	t.Run("nested torrent folder", func(t *testing.T) {
		f, variantID, audioFileID, sourceDir, sourceTrack := newRejectSourceAudioOverlapFixture(t)
		nestedDir := filepath.Join(sourceDir, "nested")
		nestedTrack := filepath.Join(nestedDir, filepath.Base(sourceTrack))
		if err := os.MkdirAll(nestedDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(sourceTrack, nestedTrack); err != nil {
			t.Fatal(err)
		}
		f.qbit.torrents[0].ContentPath = "/qbit/data/recording/nested"
		f.qbit.files = []qbittorrent.File{{Name: "recording/nested/01.m4b", Size: int64(len("source audiobook"))}}
		body := map[string]any{"downloadId": f.download.ID, "bookFileId": audioFileID, "reason": "wrong language"}
		rec := f.post(t, f.book.ID, variantID, body, auth.RoleAdmin)
		if rec.Code != http.StatusConflict {
			t.Fatalf("nested overlap: %d %s; want 409", rec.Code, rec.Body.String())
		}
		if rejected, err := f.rejected.IsRejected(f.ctx, f.book.ID, variantID, nestedDir); err != nil || rejected {
			t.Fatalf("nested overlap was rejected: %t %v", rejected, err)
		}
	})
}

func TestRejectSourceAudiobookRequiresEveryMediaMember(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, string, string)
	}{
		{"different inode for second track", func(t *testing.T, sourceDir, _ string) {
			track := filepath.Join(sourceDir, "disc2", "02.mp3")
			if err := os.Remove(track); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(track, []byte("not the tracked audio"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"unlisted foreign media in content folder", func(t *testing.T, sourceDir, _ string) {
			if err := os.WriteFile(filepath.Join(sourceDir, "foreign.m4b"), []byte("foreign"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink in tracked library folder", func(t *testing.T, _, libraryDir string) {
			if err := os.Symlink(filepath.Join(libraryDir, "01.m4b"), filepath.Join(libraryDir, "other.m4b")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, variantID, audioFileID, sourceDir, libraryDir := newRejectSourceAudioFixture(t)
			tc.change(t, sourceDir, libraryDir)
			body := map[string]any{"downloadId": f.download.ID, "bookFileId": audioFileID, "reason": "wrong language"}
			rec := f.post(t, f.book.ID, variantID, body, auth.RoleAdmin)
			if rec.Code != http.StatusConflict {
				t.Fatalf("audiobook with unproved member = %d %s; want 409", rec.Code, rec.Body.String())
			}
			if rejected, err := f.rejected.IsRejected(f.ctx, f.book.ID, variantID, sourceDir); err != nil || rejected {
				t.Fatalf("unproved audiobook source was rejected: %t %v", rejected, err)
			}
		})
	}
}

// A foreign release may be hard-linked into the library and sit beside the
// recorded release. A directory walk would reject the foreign file, even
// though the identified torrent lists only dune.epub.
func TestRejectSourceForeignFileInSameSaveDirectory(t *testing.T) {
	f := newRejectSourceFixture(t)
	if err := os.Remove(f.source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.source, []byte("actual torrent file"), 0o644); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(filepath.Dir(f.source), "foreign.epub")
	if err := os.Link(f.library, foreign); err != nil {
		t.Skipf("hardlinks unavailable on test volume: %v", err)
	}
	rec := f.post(t, f.book.ID, f.variant.ID, f.body(), auth.RoleAdmin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("foreign file: %d %s; want 409", rec.Code, rec.Body.String())
	}
	if rejected, err := f.rejected.IsRejected(f.ctx, f.book.ID, f.variant.ID, foreign); err != nil || rejected {
		t.Fatalf("foreign release was rejected: %t %v", rejected, err)
	}
}

func TestRejectSourceRefusesCallerSuppliedSourcePath(t *testing.T) {
	f := newRejectSourceFixture(t)
	body := f.body()
	body["sourcePath"] = f.source
	rec := f.post(t, f.book.ID, f.variant.ID, body, auth.RoleAdmin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("caller supplied source = %d %s; want 400", rec.Code, rec.Body.String())
	}
	f.assertNoRejection(t)
}

func TestRejectSourceRejectsMalformedIDsAndReason(t *testing.T) {
	f := newRejectSourceFixture(t)
	for _, tc := range []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"downloadId": f.download.ID, "bookFileId": f.file.ID, "reason": "  "}, http.StatusBadRequest},
		{map[string]any{"downloadId": f.download.ID, "bookFileId": 0, "reason": "wrong"}, http.StatusBadRequest},
	} {
		rec := f.post(t, f.book.ID, f.variant.ID, tc.body, auth.RoleAdmin)
		if rec.Code != tc.want {
			t.Errorf("body %v = %d %s", tc.body, rec.Code, rec.Body.String())
		}
	}
	f.assertNoRejection(t)
}
