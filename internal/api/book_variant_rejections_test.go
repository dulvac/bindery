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
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/db"
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
	dl := &models.Download{GUID: "dune-romanian", BookID: &book.ID, Title: "Dune by Frank Herbert [ROM / EPUB]", Status: models.StateImported, Protocol: "torrent"}
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
	h := NewBookHandler(books, nil, nil, nil).WithDownloads(db.NewDownloadRepo(conn)).WithRoots(NewLibraryRoots(nil, filepath.Dir(library))).WithVariantRejections(rejected, root)
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) { RegisterVariantRejectionRoute(r, h) })
	return &rejectSourceFixture{conn: conn, database: books, router: r, book: book, variant: variants[1], download: dl,
		file: files[0], source: source, library: library, root: root, ctx: ctx, rejected: rejected}
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
	got, err := f.rejected.IsRejected(f.ctx, f.book.ID, f.source)
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
	got, err := f.rejected.IsRejected(f.ctx, f.book.ID, f.source)
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
		{"missing persisted import path", func(t *testing.T, f *rejectSourceFixture) {
			if err := db.NewDownloadRepo(f.conn).SetImportPath(f.ctx, f.download.ID, ""); err != nil {
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
		{"source outside root", func(t *testing.T, f *rejectSourceFixture) {
			if err := db.NewDownloadRepo(f.conn).SetImportPath(f.ctx, f.download.ID, filepath.Dir(f.library)); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink source", func(t *testing.T, f *rejectSourceFixture) {
			if err := os.Remove(f.source); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.library, f.source); err != nil {
				t.Fatal(err)
			}
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

// A source root by itself is not a completed release identity. A path equal
// to the library file is not proof of a separate hardlink, even when both
// configured roots overlap.
func TestRejectSourceRefusesBroadRootAndSelfMatch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		root      func(*rejectSourceFixture) string
		persisted func(*rejectSourceFixture) string
	}{
		{"download root instead of release", func(f *rejectSourceFixture) string { return f.root }, func(f *rejectSourceFixture) string { return f.root }},
		{"source is library path", func(f *rejectSourceFixture) string { return filepath.Dir(f.library) }, func(f *rejectSourceFixture) string { return f.library }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRejectSourceFixture(t)
			if err := db.NewDownloadRepo(f.conn).SetImportPath(f.ctx, f.download.ID, tc.persisted(f)); err != nil {
				t.Fatal(err)
			}
			h := NewBookHandler(f.database, nil, nil, nil).WithDownloads(db.NewDownloadRepo(f.conn)).WithRoots(NewLibraryRoots(nil, filepath.Dir(f.library))).WithVariantRejections(f.rejected, tc.root(f))
			r := chi.NewRouter()
			r.Route("/api/v1", func(r chi.Router) { RegisterVariantRejectionRoute(r, h) })
			f.router = r
			rec := f.post(t, f.book.ID, f.variant.ID, f.body(), auth.RoleAdmin)
			if rec.Code != http.StatusConflict {
				t.Fatalf("%d %s; want 409", rec.Code, rec.Body.String())
			}
			f.assertNoRejection(t)
		})
	}
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
			if err := db.NewBookFileRepo(f.conn).SetVariant(f.ctx, f.library, &english, "", "unverified"); err != nil {
				t.Fatal(err)
			}
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

func TestRejectSourceAcceptsProvenSingleFileImportPath(t *testing.T) {
	f := newRejectSourceFixture(t)
	if err := db.NewDownloadRepo(f.conn).SetImportPath(f.ctx, f.download.ID, f.source); err != nil {
		t.Fatal(err)
	}
	rec := f.post(t, f.book.ID, f.variant.ID, f.body(), auth.RoleAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("single-file source = %d %s", rec.Code, rec.Body.String())
	}
	if rejected, err := f.rejected.IsRejected(f.ctx, f.book.ID, f.source); err != nil || !rejected {
		t.Fatalf("single-file source not recorded: %t %v", rejected, err)
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
