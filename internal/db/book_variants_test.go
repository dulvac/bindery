package db

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// A second request with reversed/aliased languages must not make new variants
// or lose an existing variant's status.
func TestEnsureWantedLanguagesIsIdempotent(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	repo := NewBookVariantRepo(database)
	first, err := repo.EnsureWanted(ctx, book.ID, "ebook", []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].Language != "eng" || first[1].Language != "rum" || first[0].ID == first[1].ID || first[0].Status != "wanted" {
		t.Fatalf("initial ebook variants = %#v", first)
	}
	if err := repo.SetStatus(ctx, first[0].ID, "satisfied"); err != nil {
		t.Fatal(err)
	}
	again, err := repo.EnsureWanted(ctx, book.ID, "ebook", []string{"ron", "EN", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 || first[0].ID != again[0].ID || first[1].ID != again[1].ID || again[0].Status != "satisfied" {
		t.Fatalf("not idempotent: first=%#v again=%#v", first, again)
	}
	wanted, err := repo.ListWanted(ctx, book.ID)
	if err != nil || len(wanted) != 1 || wanted[0].ID != first[1].ID {
		t.Fatalf("wanted = %#v, %v; want Romanian only", wanted, err)
	}
}

func TestNormalizeRequestLanguages(t *testing.T) {
	input := []string{" RO ", "ron", "en-US", "ENG", "", "deu"}
	if got := models.NormalizeRequestLanguages(input); !reflect.DeepEqual(got, []string{"eng", "ger", "rum"}) {
		t.Fatalf("normalized = %v; want [eng ger rum]", got)
	}
	if !reflect.DeepEqual(input, []string{" RO ", "ron", "en-US", "ENG", "", "deu"}) {
		t.Fatalf("input changed: %v", input)
	}
	if got := models.NormalizeRequestLanguages(nil); !reflect.DeepEqual(got, []string{"eng"}) {
		t.Fatalf("default = %v; want [eng]", got)
	}
}

func TestBookVariantRepoSeparatesMediaTypes(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	repo := NewBookVariantRepo(database)
	books, err := repo.EnsureWanted(ctx, book.ID, "ebook", []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	audio, err := repo.EnsureWanted(ctx, book.ID, "audiobook", []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 1 || len(audio) != 1 || books[0].ID == audio[0].ID {
		t.Fatalf("independent media variants: ebooks=%#v audio=%#v", books, audio)
	}
	all, err := repo.ListByBook(ctx, book.ID)
	if err != nil || len(all) != 2 || all[0].MediaType != "audiobook" || all[1].MediaType != "ebook" {
		t.Fatalf("ListByBook = %#v, %v", all, err)
	}
	if _, err := repo.EnsureWanted(ctx, book.ID, "magazine", []string{"eng"}); err == nil {
		t.Fatal("invalid media type created a variant")
	}
	all, err = repo.ListByBook(ctx, book.ID)
	if err != nil || len(all) != 2 {
		t.Fatalf("invalid call changed variants: %#v, %v", all, err)
	}
}

// This seeds a version-114 schema and file before running the normal upgrade.
// A migration that rebuilds or backfills book_files would discard or mislabel
// the existing path, which is not a verified language claim.
func TestVariantMigrationPreservesLegacyFile(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := setPragmas(database); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		version, err := migrationVersion(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if version >= 115 {
			continue
		}
		if entry.Name() == "025_multiuser.sql" {
			if err := multiuserPreFlight(database); err != nil {
				t.Fatal(err)
			}
		}
		body, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if err := applyMigration(database, version, entry.Name(), string(body)); err != nil {
			t.Fatalf("apply %s: %v", entry.Name(), err)
		}
	}
	ctx := context.Background()
	author := &models.Author{ForeignID: "legacy-author", Name: "Legacy", SortName: "Legacy"}
	if err := NewAuthorRepo(database).Create(ctx, author); err != nil {
		t.Fatal(err)
	}
	book := &models.Book{ForeignID: "legacy-book", AuthorID: author.ID, Title: "Legacy", SortTitle: "Legacy", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := NewBookRepo(database).Create(ctx, book); err != nil {
		t.Fatal(err)
	}
	res, err := database.ExecContext(ctx, `INSERT INTO book_files (book_id, format, path) VALUES (?, 'ebook', '/lib/legacy.epub')`, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	res, err = database.ExecContext(ctx, `INSERT INTO downloads (guid, book_id, title, nzb_url) VALUES ('old-guid', ?, 'Old', '')`, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	downloadID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(database); err != nil {
		t.Fatalf("upgrade to 115: %v", err)
	}
	var path, language, verification string
	var variantID sql.NullInt64
	if err := database.QueryRowContext(ctx, `SELECT path, variant_id, language, verification FROM book_files WHERE id = ?`, fileID).Scan(&path, &variantID, &language, &verification); err != nil {
		t.Fatal(err)
	}
	if path != "/lib/legacy.epub" || variantID.Valid || language != "" || verification != "unverified" {
		t.Fatalf("migrated legacy file = (%q, %v, %q, %q)", path, variantID, language, verification)
	}
	file, err := NewBookFileRepo(database).GetByPath(ctx, path)
	if err != nil || file == nil || file.ID != fileID || file.VariantID != nil || file.Language != "" || file.Verification != "unverified" {
		t.Fatalf("legacy file through repo = %#v, %v", file, err)
	}
	download, err := NewDownloadRepo(database).GetByID(ctx, downloadID)
	if err != nil || download == nil || download.VariantID != nil {
		t.Fatalf("legacy download through repo = %#v, %v", download, err)
	}

	// A Down is not a substitute for restoring the pre-migration backup, but
	// running it must not delete any legacy book_files rows.
	body, err := migrationsFS.ReadFile("migrations/115_language_variants.sql")
	if err != nil {
		t.Fatal(err)
	}
	index := strings.Index(string(body), "-- +migrate Down")
	if index < 0 {
		t.Fatal("migration 115 has no Down section")
	}
	for _, stmt := range splitSQLStatements(string(body)[index+len("-- +migrate Down"):]) {
		if _, err := database.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("rollback statement %q: %v", stmt, err)
		}
	}
	var afterPath string
	if err := database.QueryRowContext(ctx, `SELECT path FROM book_files WHERE id = ?`, fileID).Scan(&afterPath); err != nil || afterPath != path {
		t.Fatalf("legacy file after Down = %q, %v", afterPath, err)
	}
}

func TestVariantAssociationsRoundTripThroughFileAndDownloadRepos(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	variants, err := NewBookVariantRepo(database).EnsureWanted(ctx, book.ID, "ebook", []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	variantID := variants[0].ID
	files := NewBookFileRepo(database)
	if err := files.Add(ctx, book.ID, "ebook", "/lib/romanian.epub"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE book_files SET variant_id = ?, language = 'rum', verification = 'verified' WHERE path = '/lib/romanian.epub'`, variantID); err != nil {
		t.Fatal(err)
	}
	checkFile := func(t *testing.T, f models.BookFile) {
		t.Helper()
		if f.VariantID == nil || *f.VariantID != variantID || f.Language != "rum" || f.Verification != "verified" {
			t.Fatalf("file association = %#v", f)
		}
	}
	byPath, err := files.GetByPath(ctx, "/lib/romanian.epub")
	if err != nil || byPath == nil {
		t.Fatalf("GetByPath = %#v, %v", byPath, err)
	}
	checkFile(t, *byPath)
	byBook, err := files.ListByBook(ctx, book.ID)
	if err != nil || len(byBook) != 1 {
		t.Fatalf("ListByBook = %#v, %v", byBook, err)
	}
	checkFile(t, byBook[0])
	byBooks, err := files.ListByBooks(ctx, []int64{book.ID})
	if err != nil || len(byBooks[book.ID]) != 1 {
		t.Fatalf("ListByBooks = %#v, %v", byBooks, err)
	}
	checkFile(t, byBooks[book.ID][0])

	downloads := NewDownloadRepo(database)
	d := &models.Download{GUID: "variant-guid", BookID: &book.ID, VariantID: &variantID, Title: "Romanian", Status: models.StateGrabbed, Protocol: "usenet"}
	if err := downloads.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	got, err := downloads.GetByID(ctx, d.ID)
	if err != nil || got == nil || got.VariantID == nil || *got.VariantID != variantID {
		t.Fatalf("GetByID = %#v, %v", got, err)
	}
	listed, err := downloads.List(ctx)
	if err != nil || len(listed) != 1 || listed[0].VariantID == nil || *listed[0].VariantID != variantID {
		t.Fatalf("List downloads = %#v, %v", listed, err)
	}
}

func TestDownloadRegrabUpdatesVariantAssociation(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	variants, err := NewBookVariantRepo(database).EnsureWanted(ctx, book.ID, "ebook", []string{"rum", "eng"})
	if err != nil {
		t.Fatal(err)
	}
	downloads := NewDownloadRepo(database)
	oldVariantID := variants[1].ID // Romanian
	d := &models.Download{GUID: "regrab-variant", BookID: &book.ID, VariantID: &oldVariantID, Title: "Old", Status: models.StateFailed, Protocol: "usenet"}
	if err := downloads.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	newVariantID := variants[0].ID // English
	d.VariantID = &newVariantID
	claimed, err := downloads.RetryFailed(ctx, d)
	if err != nil || !claimed {
		t.Fatalf("RetryFailed claimed = %t, %v", claimed, err)
	}
	got, err := downloads.GetByID(ctx, d.ID)
	if err != nil || got == nil || got.VariantID == nil || *got.VariantID != newVariantID {
		t.Fatalf("regrab variant = %#v, %v", got, err)
	}
}

// A variant belongs to its book. Explicitly moving or reclaiming a file for
// another book must not carry a language claim from the former owner.
func TestBookFileOwnershipChangeClearsVariant(t *testing.T) {
	for _, mode := range []string{"move", "reclaim"} {
		t.Run(mode, func(t *testing.T) {
			database, author, book := openTestDB(t)
			ctx := context.Background()
			other := &models.Book{ForeignID: "other-book-" + mode, AuthorID: author.ID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
			if err := NewBookRepo(database).Create(ctx, other); err != nil {
				t.Fatal(err)
			}
			variants, err := NewBookVariantRepo(database).EnsureWanted(ctx, book.ID, "ebook", []string{"rum"})
			if err != nil {
				t.Fatal(err)
			}
			files := NewBookFileRepo(database)
			path := "/lib/ownership-" + mode + ".epub"
			if err := files.Add(ctx, book.ID, "ebook", path); err != nil {
				t.Fatal(err)
			}
			if _, err := database.ExecContext(ctx, `UPDATE book_files SET variant_id = ?, language = 'rum', verification = 'verified' WHERE path = ?`, variants[0].ID, path); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "move":
				if _, err := files.MoveToBook(ctx, other.ID, "ebook", path); err != nil {
					t.Fatal(err)
				}
			case "reclaim":
				if _, err := database.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
					t.Fatal(err)
				}
				if _, err := database.ExecContext(ctx, `UPDATE book_files SET book_id = 999999 WHERE path = ?`, path); err != nil {
					t.Fatal(err)
				}
				if _, err := database.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
					t.Fatal(err)
				}
				result, err := files.Track(ctx, other.ID, "ebook", path)
				if err != nil || result.Outcome != TrackReclaimedOrphan {
					t.Fatalf("Track reclaim = %#v, %v", result, err)
				}
			}
			file, err := files.GetByPath(ctx, path)
			if err != nil || file == nil || file.BookID != other.ID || file.VariantID != nil || file.Language != "" || file.Verification != "unverified" {
				t.Fatalf("file after %s = %#v, %v", mode, file, err)
			}
		})
	}
}

func TestDownloadBookReassignmentClearsVariant(t *testing.T) {
	database, author, book := openTestDB(t)
	ctx := context.Background()
	other := &models.Book{ForeignID: "download-new-book", AuthorID: author.ID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := NewBookRepo(database).Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	variants, err := NewBookVariantRepo(database).EnsureWanted(ctx, book.ID, "ebook", []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewDownloadRepo(database)
	d := &models.Download{GUID: "reassign-variant", BookID: &book.ID, VariantID: &variants[0].ID, Title: "Old", Status: models.StateGrabbed, Protocol: "usenet"}
	if err := repo.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetBookID(ctx, d.ID, book.ID); err != nil {
		t.Fatal(err)
	}
	unchanged, err := repo.GetByID(ctx, d.ID)
	if err != nil || unchanged == nil || unchanged.VariantID == nil || *unchanged.VariantID != variants[0].ID {
		t.Fatalf("same-book reassignment lost the variant: %#v, %v", unchanged, err)
	}
	if err := repo.SetBookID(ctx, d.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetByID(ctx, d.ID)
	if err != nil || got == nil || got.BookID == nil || *got.BookID != other.ID || got.VariantID != nil {
		t.Fatalf("reassigned download = %#v, %v", got, err)
	}
}
