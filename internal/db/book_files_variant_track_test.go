package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

func variantStatus(t *testing.T, database *sql.DB, bookID, variantID int64) string {
	t.Helper()
	rows, err := NewBookVariantRepo(database).ListByBook(context.Background(), bookID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == variantID {
			return row.Status
		}
	}
	t.Fatalf("variant %d missing from book %d", variantID, bookID)
	return ""
}

// A language-bearing path is tracked with its actual evidence, not the work's
// catalogue language. Both copies remain visible through the file list.
func TestTrackVariantTwoLanguagesAndCompatibilityPath(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	books := NewBookRepo(database)
	files := NewBookFileRepo(database)
	if err := books.SetLanguage(ctx, book.ID, "eng"); err != nil {
		t.Fatal(err)
	}
	rows, err := books.VariantRepo().EnsureWanted(ctx, book.ID, "ebook", []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	english, romanian := filepath.Join(root, "Dune [eng].epub"), filepath.Join(root, "Dune [rum].epub")
	for _, path := range []string{english, romanian} {
		if err := os.WriteFile(path, []byte("placed copy"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := files.TrackVariant(ctx, book.ID, rows[0].ID, "ebook", english, "eng", "verified"); err != nil {
		t.Fatal(err)
	}
	// An unresolved source belongs to the requested variant, but its file
	// language stays unknown, even when books.language says English.
	if err := files.TrackVariant(ctx, book.ID, rows[1].ID, "ebook", romanian, "", "unverified"); err != nil {
		t.Fatal(err)
	}
	for _, variant := range rows {
		if got := variantStatus(t, database, book.ID, variant.ID); got != models.VariantImported {
			t.Fatalf("%s variant status = %q; want imported", variant.Language, got)
		}
	}
	tracked, err := files.ListByBook(ctx, book.ID)
	if err != nil || len(tracked) != 2 || tracked[0].Path != english || tracked[0].Language != "eng" || tracked[0].Verification != "verified" || tracked[1].Path != romanian || tracked[1].Language != "" || tracked[1].Verification != "unverified" {
		t.Fatalf("tracked copies = %#v, %v", tracked, err)
	}
	got, err := books.GetByID(ctx, book.ID)
	if err != nil || got == nil || got.Language != "eng" || got.Status != models.BookStatusImported || got.EbookFilePath != english || got.FilePath != english {
		t.Fatalf("compatibility book = %#v, %v", got, err)
	}
	if err := files.TrackVariant(ctx, book.ID, rows[1].ID, "ebook", romanian, "", "unverified"); err != nil {
		t.Fatalf("identical retry should be idempotent: %v", err)
	}
	tracked, err = files.ListByBook(ctx, book.ID)
	if err != nil || len(tracked) != 2 {
		t.Fatalf("retry created another row: %#v, %v", tracked, err)
	}
}

func TestTrackVariantRejectsWrongAssociationAndExistingPath(t *testing.T) {
	database, author, book := openTestDB(t)
	ctx := context.Background()
	books := NewBookRepo(database)
	files := NewBookFileRepo(database)
	other := &models.Book{ForeignID: "track-variant-other", AuthorID: author.ID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := books.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	variants := books.VariantRepo()
	own, err := variants.EnsureWanted(ctx, book.ID, "ebook", []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	audio, err := variants.EnsureWanted(ctx, book.ID, "audiobook", []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := variants.EnsureWanted(ctx, other.ID, "ebook", []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	const path = "/library/books/Title [eng].epub"
	if err := files.TrackVariant(ctx, book.ID, own[0].ID, "ebook", path, "eng", "verified"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		bookID, id   int64
		format, path string
		language     string
		verification string
	}{
		{"same-path other language", book.ID, own[1].ID, "ebook", path, "rum", "verified"},
		{"foreign book", book.ID, foreign[0].ID, "ebook", "/lib/foreign.epub", "rum", "verified"},
		{"wrong format", book.ID, audio[0].ID, "ebook", "/lib/audio.epub", "rum", "verified"},
		{"wrong book id", other.ID, own[1].ID, "ebook", "/lib/wrong-book.epub", "rum", "verified"},
		{"verified wrong language", book.ID, own[1].ID, "ebook", "/lib/wrong-language.epub", "eng", "verified"},
		{"unverified known mismatch", book.ID, own[1].ID, "ebook", "/lib/unverified-mismatch.epub", "eng", "unverified"},
		{"unknown verification", book.ID, own[1].ID, "ebook", "/lib/unknown.epub", "rum", "unknown"},
		{"verified unknown language", book.ID, own[1].ID, "ebook", "/lib/empty.epub", "", "verified"},
		{"non-absolute path", book.ID, own[1].ID, "ebook", "../lib/unsafe.epub", "rum", "verified"},
		{"audio file disguised as ebook", book.ID, own[1].ID, "ebook", "/lib/audio.m4b", "rum", "verified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := files.TrackVariant(ctx, tc.bookID, tc.id, tc.format, tc.path, tc.language, tc.verification); err == nil {
				t.Fatal("accepted an invalid variant file claim")
			}
			if got, err := files.GetByPath(ctx, tc.path); err != nil || (tc.path != path && got != nil) {
				t.Fatalf("rejected claim left a file row: %#v, %v", got, err)
			}
		})
	}
	if got := variantStatus(t, database, book.ID, own[1].ID); got != models.VariantWanted {
		t.Fatalf("rejected variant status = %q", got)
	}
	if got, err := files.GetByPath(ctx, path); err != nil || got == nil || got.VariantID == nil || *got.VariantID != own[0].ID {
		t.Fatalf("collision displaced first owner: %#v, %v", got, err)
	}
	// An old row is never silently relabeled as a requested language.
	const legacy = "/lib/existing-legacy.epub"
	if err := files.Add(ctx, book.ID, "ebook", legacy); err != nil {
		t.Fatal(err)
	}
	if err := files.TrackVariant(ctx, book.ID, own[1].ID, "ebook", legacy, "rum", "verified"); err == nil {
		t.Fatal("claimed an existing legacy path")
	}
	got, err := files.GetByPath(ctx, legacy)
	if err != nil || got == nil || got.VariantID != nil || got.Language != "" || got.Verification != "unverified" {
		t.Fatalf("legacy row changed: %#v, %v", got, err)
	}
	// A path held by another book remains owned by it.
	const otherPath = "/lib/other-book [rum].epub"
	if err := files.TrackVariant(ctx, other.ID, foreign[0].ID, "ebook", otherPath, "rum", "verified"); err != nil {
		t.Fatal(err)
	}
	err = files.TrackVariant(ctx, book.ID, own[1].ID, "ebook", otherPath, "rum", "verified")
	var owned *PathOwnedError
	if !errors.As(err, &owned) || owned.OwnerBookID != other.ID {
		t.Fatalf("cross-book path error = %v; want PathOwnedError for book %d", err, other.ID)
	}
}

func TestTrackVariantRetryRejectsExistingFileInAnotherFormat(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	rows, err := NewBookVariantRepo(database).EnsureWanted(ctx, book.ID, "ebook", []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	const path = "/lib/format-drift [rum].epub"
	files := NewBookFileRepo(database)
	if err := files.TrackVariant(ctx, book.ID, rows[0].ID, "ebook", path, "rum", "verified"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE book_files SET format = 'audiobook' WHERE path = ?`, path); err != nil {
		t.Fatal(err)
	}
	if err := NewBookVariantRepo(database).SetStatus(ctx, rows[0].ID, models.VariantWanted); err != nil {
		t.Fatal(err)
	}
	if err := files.TrackVariant(ctx, book.ID, rows[0].ID, "ebook", path, "rum", "verified"); err == nil {
		t.Fatal("retry imported a variant using a row with the wrong format")
	}
	if got := variantStatus(t, database, book.ID, rows[0].ID); got != models.VariantWanted {
		t.Fatalf("failed retry changed status to %q", got)
	}
}

func TestSetVariantCannotAttachUnverifiedWrongLanguage(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	rows, err := NewBookVariantRepo(database).EnsureWanted(ctx, book.ID, "ebook", []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	files := NewBookFileRepo(database)
	const path = "/lib/unknown-language.epub"
	if err := files.Add(ctx, book.ID, "ebook", path); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ language, verification string }{{"eng", "unverified"}, {"rum", "guess"}, {"", "verified"}} {
		if err := files.SetVariant(ctx, path, &rows[0].ID, tc.language, tc.verification); err == nil {
			t.Fatalf("SetVariant accepted %q / %q", tc.language, tc.verification)
		}
		if got, err := files.GetByPath(ctx, path); err != nil || got == nil || got.VariantID != nil || got.Language != "" || got.Verification != "unverified" {
			t.Fatalf("invalid association changed file: %#v, %v", got, err)
		}
	}
	if err := files.SetVariant(ctx, path, &rows[0].ID, "", "unverified"); err != nil {
		t.Fatalf("unknown language must remain unlabeled: %v", err)
	}
}

func TestTrackVariantReviewNeedsExplicitRetry(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	variants := NewBookVariantRepo(database)
	rows, err := variants.EnsureWanted(ctx, book.ID, "ebook", []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	if err := variants.SetStatus(ctx, rows[0].ID, models.VariantReview); err != nil {
		t.Fatal(err)
	}
	files := NewBookFileRepo(database)
	const path = "/lib/pending-review.epub"
	if err := files.TrackVariant(ctx, book.ID, rows[0].ID, "ebook", path, "eng", "verified"); err == nil {
		t.Fatal("implicit retry cleared manual review")
	}
	if got := variantStatus(t, database, book.ID, rows[0].ID); got != models.VariantReview {
		t.Fatalf("review status = %q", got)
	}
	if got, err := files.GetByPath(ctx, path); err != nil || got != nil {
		t.Fatalf("review path unexpectedly tracked: %#v, %v", got, err)
	}
}

func TestTrackVariantDBFailureRollsBackAndKeepsPlacedPath(t *testing.T) {
	for _, failure := range []string{"variant status", "book compatibility"} {
		t.Run(failure, func(t *testing.T) {
			database, _, book := openTestDB(t)
			ctx := context.Background()
			rows, err := NewBookVariantRepo(database).EnsureWanted(ctx, book.ID, "ebook", []string{"rum"})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "safe [rum].epub")
			if err := os.WriteFile(path, []byte("existing placed link"), 0600); err != nil {
				t.Fatal(err)
			}
			trigger := `CREATE TRIGGER reject_variant_import BEFORE UPDATE OF status ON book_variants
				WHEN NEW.status = 'imported' BEGIN SELECT RAISE(ABORT, 'blocked'); END`
			if failure == "book compatibility" {
				trigger = `CREATE TRIGGER reject_book_refresh BEFORE UPDATE OF ebook_file_path ON books
					BEGIN SELECT RAISE(ABORT, 'blocked'); END`
			}
			if _, err := database.ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			if err := NewBookFileRepo(database).TrackVariant(ctx, book.ID, rows[0].ID, "ebook", path, "rum", "verified"); err == nil {
				t.Fatal("expected DB failure")
			}
			if got, err := NewBookFileRepo(database).GetByPath(ctx, path); err != nil || got != nil {
				t.Fatalf("failed import kept file association: %#v, %v", got, err)
			}
			if got := variantStatus(t, database, book.ID, rows[0].ID); got != models.VariantWanted {
				t.Fatalf("failed import status = %q", got)
			}
			var pathColumn string
			if err := database.QueryRowContext(ctx, `SELECT COALESCE(ebook_file_path, '') FROM books WHERE id = ?`, book.ID).Scan(&pathColumn); err != nil || pathColumn != "" {
				t.Fatalf("failed import compatibility path = %q, %v", pathColumn, err)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "existing placed link" {
				t.Fatalf("DB failure unlinked or altered public path: %q, %v", data, err)
			}
		})
	}
}

func TestTrackVariantLastFileReconcilesAcrossRemovalPaths(t *testing.T) {
	for _, op := range []string{"delete by path", "delete by book", "remove book file", "untrack", "move folder", "reclaim"} {
		t.Run(op, func(t *testing.T) {
			database, author, book := openTestDB(t)
			ctx := context.Background()
			books, files := NewBookRepo(database), NewBookFileRepo(database)
			other := &models.Book{ForeignID: "track-reassign-" + op, AuthorID: author.ID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeAudiobook, Status: models.BookStatusWanted}
			if err := books.Create(ctx, other); err != nil {
				t.Fatal(err)
			}
			rows, err := books.VariantRepo().EnsureWanted(ctx, book.ID, "audiobook", []string{"fre"})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "Title [fre]")
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := files.TrackVariant(ctx, book.ID, rows[0].ID, "audiobook", path, "", "unverified"); err != nil {
				t.Fatal(err)
			}
			switch op {
			case "delete by path":
				_, err = files.DeleteByPath(ctx, path)
			case "delete by book":
				err = files.DeleteByBook(ctx, book.ID)
			case "remove book file":
				_, err = books.RemoveBookFile(ctx, path)
			case "untrack":
				_, err = books.UntrackFilePath(ctx, path)
			case "move folder":
				_, err = files.MoveToBook(ctx, other.ID, "audiobook", path)
			case "reclaim":
				if _, err = database.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
					t.Fatal(err)
				}
				if _, err = database.ExecContext(ctx, `UPDATE book_files SET book_id = 999999 WHERE path = ?`, path); err != nil {
					t.Fatal(err)
				}
				if _, err = database.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
					t.Fatal(err)
				}
				_, err = files.Track(ctx, other.ID, "audiobook", path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := variantStatus(t, database, book.ID, rows[0].ID); got != models.VariantWanted {
				t.Fatalf("last unverified folder removed by %s: status = %q", op, got)
			}
			if op == "remove book file" || op == "untrack" {
				got, err := books.GetByID(ctx, book.ID)
				if err != nil || got == nil || got.AudiobookFilePath != "" {
					t.Fatalf("compatibility path after %s = %#v, %v", op, got, err)
				}
			}
		})
	}
}

func TestUntrackVariantFileForBookReopensVariant(t *testing.T) {
	database, author, book := openTestDB(t)
	ctx := context.Background()
	books, files := NewBookRepo(database), NewBookFileRepo(database)
	other := &models.Book{ForeignID: "adoption-undo-other", AuthorID: author.ID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := books.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	rows, err := books.VariantRepo().EnsureWanted(ctx, book.ID, "ebook", []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	const path = "/lib/adopted [rum].epub"
	if err := files.TrackVariant(ctx, book.ID, rows[0].ID, "ebook", path, "", "unverified"); err != nil {
		t.Fatal(err)
	}
	removed, err := books.UntrackFilePathForBook(ctx, path, other.ID)
	if err != nil || removed {
		t.Fatalf("wrong book untrack = %t, %v", removed, err)
	}
	removed, err = books.UntrackFilePathForBook(ctx, path, book.ID)
	if err != nil || !removed {
		t.Fatalf("owner untrack = %t, %v", removed, err)
	}
	if got := variantStatus(t, database, book.ID, rows[0].ID); got != models.VariantWanted {
		t.Fatalf("undo left unverified variant %q", got)
	}
	if got, err := books.GetByID(ctx, book.ID); err != nil || got == nil || got.EbookFilePath != "" {
		t.Fatalf("undo kept compatibility path: %#v, %v", got, err)
	}
}

func TestVariantFileRemovalKeepsSiblingOrManualReview(t *testing.T) {
	for _, mode := range []string{"sibling", "review"} {
		t.Run(mode, func(t *testing.T) {
			database, _, book := openTestDB(t)
			ctx := context.Background()
			files, variants := NewBookFileRepo(database), NewBookVariantRepo(database)
			rows, err := variants.EnsureWanted(ctx, book.ID, "ebook", []string{"rum"})
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/lib/first [rum].epub", "/lib/second [rum].epub"} {
				if err := files.TrackVariant(ctx, book.ID, rows[0].ID, "ebook", path, "", "unverified"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "review" {
				if err := variants.SetStatus(ctx, rows[0].ID, models.VariantReview); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := files.DeleteByPath(ctx, "/lib/first [rum].epub"); err != nil {
				t.Fatal(err)
			}
			want := models.VariantImported
			if mode == "review" {
				want = models.VariantReview
			}
			if got := variantStatus(t, database, book.ID, rows[0].ID); got != want {
				t.Fatalf("remaining claim status = %q; want %q", got, want)
			}
			if _, err := files.DeleteByPath(ctx, "/lib/second [rum].epub"); err != nil {
				t.Fatal(err)
			}
			want = models.VariantWanted
			if mode == "review" {
				want = models.VariantReview
			}
			if got := variantStatus(t, database, book.ID, rows[0].ID); got != want {
				t.Fatalf("last claim status = %q; want %q", got, want)
			}
		})
	}
}

func TestMoveBookFileFailureRollsBackVariantAndOwnership(t *testing.T) {
	database, author, book := openTestDB(t)
	ctx := context.Background()
	books, files := NewBookRepo(database), NewBookFileRepo(database)
	other := &models.Book{ForeignID: "failed-book-move", AuthorID: author.ID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := books.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	rows, err := books.VariantRepo().EnsureWanted(ctx, book.ID, "ebook", []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	const path = "/lib/failed-move [rum].epub"
	if err := files.TrackVariant(ctx, book.ID, rows[0].ID, "ebook", path, "rum", "verified"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `CREATE TRIGGER reject_target_refresh BEFORE UPDATE OF ebook_file_path ON books
		WHEN NEW.id = `+fmt.Sprint(other.ID)+` BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := books.MoveBookFile(ctx, path, other.ID, "ebook"); err == nil {
		t.Fatal("move unexpectedly succeeded with failed target refresh")
	}
	f, err := files.GetByPath(ctx, path)
	if err != nil || f == nil || f.BookID != book.ID || f.VariantID == nil || *f.VariantID != rows[0].ID {
		t.Fatalf("failed move changed association: %#v, %v", f, err)
	}
	if got := variantStatus(t, database, book.ID, rows[0].ID); got != models.VariantImported {
		t.Fatalf("failed move changed variant status = %q", got)
	}
	original, err := books.GetByID(ctx, book.ID)
	if err != nil || original == nil || original.EbookFilePath != path {
		t.Fatalf("failed move changed original book: %#v, %v", original, err)
	}
}

func TestUpdateBookFilePathFailureLeavesAssociationAndCompatibilityUnchanged(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	books, files := NewBookRepo(database), NewBookFileRepo(database)
	rows, err := books.VariantRepo().EnsureWanted(ctx, book.ID, "ebook", []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	const oldPath, newPath = "/lib/original [eng].epub", "/lib/new [eng].epub"
	if err := files.TrackVariant(ctx, book.ID, rows[0].ID, "ebook", oldPath, "eng", "verified"); err != nil {
		t.Fatal(err)
	}
	file, err := files.GetByPath(ctx, oldPath)
	if err != nil || file == nil {
		t.Fatalf("original file = %#v, %v", file, err)
	}
	epoch := files.PathEpoch()
	if _, err := database.ExecContext(ctx, `CREATE TRIGGER reject_path_refresh BEFORE UPDATE OF ebook_file_path ON books
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := books.UpdateBookFilePath(ctx, file.ID, book.ID, newPath); err == nil {
		t.Fatal("path update unexpectedly succeeded when book refresh failed")
	}
	if got, err := files.GetByPath(ctx, oldPath); err != nil || got == nil || got.VariantID == nil || *got.VariantID != rows[0].ID {
		t.Fatalf("failed update moved original association: %#v, %v", got, err)
	}
	if got, err := files.GetByPath(ctx, newPath); err != nil || got != nil {
		t.Fatalf("failed update created new association: %#v, %v", got, err)
	}
	if got := files.PathEpoch(); got != epoch {
		t.Fatalf("failed path update advanced cache epoch from %d to %d", epoch, got)
	}
	if got := variantStatus(t, database, book.ID, rows[0].ID); got != models.VariantImported {
		t.Fatalf("failed path update changed status to %q", got)
	}
}

func TestDirectVariantFileRemovalRefreshesBookCompatibility(t *testing.T) {
	for _, op := range []string{"delete by path", "delete by book"} {
		t.Run(op, func(t *testing.T) {
			database, _, book := openTestDB(t)
			ctx := context.Background()
			books, files := NewBookRepo(database), NewBookFileRepo(database)
			rows, err := books.VariantRepo().EnsureWanted(ctx, book.ID, "ebook", []string{"eng"})
			if err != nil {
				t.Fatal(err)
			}
			const path = "/lib/last [eng].epub"
			if err := files.TrackVariant(ctx, book.ID, rows[0].ID, "ebook", path, "eng", "verified"); err != nil {
				t.Fatal(err)
			}
			if op == "delete by path" {
				_, err = files.DeleteByPath(ctx, path)
			} else {
				err = files.DeleteByBook(ctx, book.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := books.GetByID(ctx, book.ID)
			if err != nil || got == nil || got.EbookFilePath != "" || got.FilePath != "" || got.Status != models.BookStatusWanted {
				t.Fatalf("book after %s still advertises deleted file: %#v, %v", op, got, err)
			}
		})
	}
}

func TestDirectVariantFileMoveRefreshesBothBooks(t *testing.T) {
	database, author, book := openTestDB(t)
	ctx := context.Background()
	books, files := NewBookRepo(database), NewBookFileRepo(database)
	other := &models.Book{ForeignID: "direct-variant-move", AuthorID: author.ID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := books.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	rows, err := books.VariantRepo().EnsureWanted(ctx, book.ID, "ebook", []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	const path = "/lib/moved [eng].epub"
	if err := files.TrackVariant(ctx, book.ID, rows[0].ID, "ebook", path, "eng", "verified"); err != nil {
		t.Fatal(err)
	}
	if _, err := files.MoveToBook(ctx, other.ID, "ebook", path); err != nil {
		t.Fatal(err)
	}
	old, err := books.GetByID(ctx, book.ID)
	if err != nil || old == nil || old.EbookFilePath != "" || old.Status != models.BookStatusWanted {
		t.Fatalf("original book after direct move = %#v, %v", old, err)
	}
	newBook, err := books.GetByID(ctx, other.ID)
	if err != nil || newBook == nil || newBook.EbookFilePath != path || newBook.Status != models.BookStatusImported {
		t.Fatalf("new book after direct move = %#v, %v", newBook, err)
	}
}

func TestDeleteVariantFileFailurePreservesAssociationAndStatus(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	rows, err := NewBookVariantRepo(database).EnsureWanted(ctx, book.ID, "ebook", []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	files := NewBookFileRepo(database)
	const path = "/lib/reconcile-error.epub"
	if err := files.TrackVariant(ctx, book.ID, rows[0].ID, "ebook", path, "eng", "verified"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `CREATE TRIGGER reject_reconcile BEFORE UPDATE OF status ON book_variants
		WHEN NEW.status = 'wanted' BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := files.DeleteByPath(ctx, path); err == nil {
		t.Fatal("delete succeeded without reconciling variant")
	}
	if got, err := files.GetByPath(ctx, path); err != nil || got == nil || got.VariantID == nil || *got.VariantID != rows[0].ID {
		t.Fatalf("failed delete dropped association: %#v, %v", got, err)
	}
	if got := variantStatus(t, database, book.ID, rows[0].ID); got != models.VariantImported {
		t.Fatalf("failed delete changed status = %q", got)
	}
}
