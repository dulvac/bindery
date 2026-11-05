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
	if err := repo.SetStatus(ctx, first[0].ID, "imported"); err != nil {
		t.Fatal(err)
	}
	again, err := repo.EnsureWanted(ctx, book.ID, "ebook", []string{"ron", "EN", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 || first[0].ID != again[0].ID || first[1].ID != again[1].ID || again[0].Status != "imported" {
		t.Fatalf("not idempotent: first=%#v again=%#v", first, again)
	}
	wanted, err := repo.ListWanted(ctx)
	if err != nil || len(wanted) != 1 || wanted[0].ID != first[1].ID {
		t.Fatalf("wanted = %#v, %v; want Romanian only", wanted, err)
	}
}

// SetStatus must accept only the four lifecycle states; an invalid state
// must not overwrite the current one.
func TestBookVariantStatusRejectsUnknownValues(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	repo := NewBookVariantRepo(database)
	variants, err := repo.EnsureWanted(ctx, book.ID, "ebook", []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	id := variants[0].ID
	for _, status := range []string{"downloading", "review", "imported", "wanted"} {
		if err := repo.SetStatus(ctx, id, status); err != nil {
			t.Fatalf("valid status %q: %v", status, err)
		}
		got, err := repo.ListByBook(ctx, book.ID)
		if err != nil || len(got) != 1 || got[0].Status != status {
			t.Fatalf("status after %q = %#v, %v", status, got, err)
		}
	}
	for _, status := range []string{"satisfied", "", "WANTED", "unknown"} {
		if err := repo.SetStatus(ctx, id, status); err == nil || !strings.Contains(err.Error(), "invalid variant status") {
			t.Errorf("SetStatus(%q) error = %v; want validation error", status, err)
		}
	}
	got, err := repo.ListByBook(ctx, book.ID)
	if err != nil || len(got) != 1 || got[0].Status != "wanted" {
		t.Fatalf("invalid status changed row: %#v, %v", got, err)
	}
}

// The database constraint also protects status when bypassing SetStatus.
func TestVariantMigrationConstrainsStatus(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	repo := NewBookVariantRepo(database)
	variants, err := repo.EnsureWanted(ctx, book.ID, "ebook", []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE book_variants SET status = 'satisfied' WHERE id = ?`, variants[0].ID); err == nil {
		t.Fatal("direct SQL update accepted a status outside the lifecycle")
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO book_variants (book_id, media_type, language, status) VALUES (?, 'ebook', 'rum', 'satisfied')`, book.ID); err == nil {
		t.Fatal("direct SQL insert accepted a status outside the lifecycle")
	}
	got, err := repo.ListByBook(ctx, book.ID)
	if err != nil || len(got) != 1 || got[0].Status != "wanted" {
		t.Fatalf("invalid SQL changed variants: %#v, %v", got, err)
	}
}

// The scheduler needs all books' wanted variants, while ListByBook retains
// the scoped view (including variants in other states).
func TestBookVariantRepoListWantedAcrossBooks(t *testing.T) {
	database, author, first := openTestDB(t)
	ctx := context.Background()
	second := &models.Book{ForeignID: "variants-second-book", AuthorID: author.ID, Title: "Second", SortTitle: "Second", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := NewBookRepo(database).Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	repo := NewBookVariantRepo(database)
	one, err := repo.EnsureWanted(ctx, first.ID, "ebook", []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	two, err := repo.EnsureWanted(ctx, second.ID, "ebook", []string{"fre"})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetStatus(ctx, one[1].ID, "imported"); err != nil {
		t.Fatal(err)
	}
	wanted, err := repo.ListWanted(ctx)
	if err != nil || len(wanted) != 2 || wanted[0].ID != one[0].ID || wanted[1].ID != two[0].ID {
		t.Fatalf("global wanted variants = %#v, %v; want English and French across books", wanted, err)
	}
	scoped, err := repo.ListByBook(ctx, first.ID)
	if err != nil || len(scoped) != 2 || scoped[1].Status != "imported" {
		t.Fatalf("scoped variants = %#v, %v", scoped, err)
	}
}

func TestNormalizeRequestLanguages(t *testing.T) {
	input := []string{" RO ", "ron", "fr", "FRA", "EN", "eng"}
	got, err := models.NormalizeRequestLanguages(input)
	if err != nil || !reflect.DeepEqual(got, []string{"eng", "fre", "rum"}) {
		t.Fatalf("normalized = %v, %v; want [eng fre rum]", got, err)
	}
	if !reflect.DeepEqual(input, []string{" RO ", "ron", "fr", "FRA", "EN", "eng"}) {
		t.Fatalf("input changed: %v", input)
	}
	got, err = models.NormalizeRequestLanguages(nil)
	if err != nil || !reflect.DeepEqual(got, []string{"eng"}) {
		t.Fatalf("default = %v, %v; want [eng]", got, err)
	}
}

func TestNormalizeRequestLanguagesRejectsExplicitEmptyOrUnsupported(t *testing.T) {
	for _, input := range [][]string{{}, {""}, {"\t"}, {"eng", ""}, {"de"}, {"ger"}, {"deu"}, {"en-US"}, {"english"}, {"rom"}} {
		if got, err := models.NormalizeRequestLanguages(input); err == nil {
			t.Errorf("NormalizeRequestLanguages(%q) = %v, nil; want error", input, got)
		}
	}
}

// A rejected language selection must not silently create an English or
// unsupported variant.
func TestEnsureWantedRejectsInvalidLanguages(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	repo := NewBookVariantRepo(database)
	for _, languages := range [][]string{{}, {""}, {"eng", "de"}, {"en-US"}} {
		if variants, err := repo.EnsureWanted(ctx, book.ID, "ebook", languages); err == nil {
			t.Errorf("EnsureWanted(%q) = %#v, nil; want error", languages, variants)
		}
	}
	got, err := repo.ListByBook(ctx, book.ID)
	if err != nil || len(got) != 0 {
		t.Fatalf("invalid requests wrote variants: %#v, %v", got, err)
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
	if err := files.SetVariant(ctx, "/lib/romanian.epub", &variantID, "rum", "verified"); err != nil {
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
			if err := files.SetVariant(ctx, path, &variants[0].ID, "rum", "verified"); err != nil {
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

// A download must not claim a variant from another book, even when both
// foreign keys independently resolve to existing rows.
func TestDownloadRepoCreateRejectsVariantFromAnotherBook(t *testing.T) {
	database, author, book := openTestDB(t)
	ctx := context.Background()
	other := &models.Book{ForeignID: "foreign-variant-book", AuthorID: author.ID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := NewBookRepo(database).Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	variants, err := NewBookVariantRepo(database).EnsureWanted(ctx, other.ID, "ebook", []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewDownloadRepo(database)
	for _, tc := range []struct {
		name   string
		bookID *int64
	}{
		{"wrong book", &book.ID},
		{"no book", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &models.Download{GUID: "foreign-create-" + tc.name, BookID: tc.bookID, VariantID: &variants[0].ID, Title: "Wrong owner", Status: models.StateGrabbed, Protocol: "usenet"}
			if err := repo.Create(ctx, d); err == nil {
				t.Fatal("Create accepted a variant without its owning book")
			}
			stored, err := repo.GetByGUID(ctx, d.GUID)
			if err != nil || stored != nil {
				t.Fatalf("rejected download was persisted: %#v, %v", stored, err)
			}
		})
	}
}

// A regrab overwrites both book_id and variant_id; it must leave the failed
// row untouched when the new book does not own the proposed variant.
func TestDownloadRepoRetryFailedRejectsVariantFromAnotherBook(t *testing.T) {
	database, author, book := openTestDB(t)
	ctx := context.Background()
	other := &models.Book{ForeignID: "regrab-foreign-book", AuthorID: author.ID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := NewBookRepo(database).Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	variants, err := NewBookVariantRepo(database).EnsureWanted(ctx, book.ID, "ebook", []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewDownloadRepo(database)
	d := &models.Download{GUID: "foreign-regrab", BookID: &book.ID, VariantID: &variants[0].ID, Title: "Original", Status: models.StateFailed, Protocol: "usenet"}
	if err := repo.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	d.BookID = &other.ID
	d.Title = "Regrab"
	if claimed, err := repo.RetryFailed(ctx, d); err == nil || claimed {
		t.Fatalf("RetryFailed claimed mismatched variant: claimed=%t, err=%v", claimed, err)
	}
	stored, err := repo.GetByID(ctx, d.ID)
	if err != nil || stored == nil || stored.BookID == nil || *stored.BookID != book.ID || stored.VariantID == nil || *stored.VariantID != variants[0].ID || stored.Title != "Original" || stored.Status != models.StateFailed {
		t.Fatalf("failed regrab changed original row: %#v, %v", stored, err)
	}
}

// Attaching a language claim to a tracked file must match both its book and
// its format. A rejected write leaves the file's metadata untouched.
func TestBookFileRepoSetVariantRejectsWrongBookAndFormat(t *testing.T) {
	database, author, book := openTestDB(t)
	ctx := context.Background()
	other := &models.Book{ForeignID: "file-foreign-variant", AuthorID: author.ID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
	if err := NewBookRepo(database).Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	variants := NewBookVariantRepo(database)
	own, err := variants.EnsureWanted(ctx, book.ID, "ebook", []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := variants.EnsureWanted(ctx, other.ID, "ebook", []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	wrongFormat, err := variants.EnsureWanted(ctx, book.ID, "audiobook", []string{"rum"})
	if err != nil {
		t.Fatal(err)
	}
	files := NewBookFileRepo(database)
	const path = "/lib/variant-ownership.epub"
	if err := files.Add(ctx, book.ID, "ebook", path); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		id   int64
	}{
		{"wrong book", foreign[0].ID},
		{"wrong format", wrongFormat[0].ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := files.SetVariant(ctx, path, &tc.id, "rum", "verified"); err == nil {
				t.Fatal("SetVariant accepted a mismatched claim")
			}
			file, err := files.GetByPath(ctx, path)
			if err != nil || file == nil || file.VariantID != nil || file.Language != "" || file.Verification != "unverified" {
				t.Fatalf("rejected claim changed file: %#v, %v", file, err)
			}
		})
	}
	if err := files.SetVariant(ctx, path, &own[0].ID, "rum", "verified"); err != nil {
		t.Fatalf("valid same-book ebook variant: %v", err)
	}
	file, err := files.GetByPath(ctx, path)
	if err != nil || file == nil || file.VariantID == nil || *file.VariantID != own[0].ID || file.Language != "rum" || file.Verification != "verified" {
		t.Fatalf("valid file association = %#v, %v", file, err)
	}
}

// A move or orphan reclaim discards the old variant claim. Without another
// usable file or active grab, an imported variant must become wanted again.
func TestBookFileOwnershipChangeReopensUnclaimedVariant(t *testing.T) {
	for _, mode := range []string{"move", "reclaim"} {
		t.Run(mode, func(t *testing.T) {
			database, author, book := openTestDB(t)
			ctx := context.Background()
			other := &models.Book{ForeignID: "reopen-file-" + mode, AuthorID: author.ID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
			if err := NewBookRepo(database).Create(ctx, other); err != nil {
				t.Fatal(err)
			}
			variants := NewBookVariantRepo(database)
			rows, err := variants.EnsureWanted(ctx, book.ID, "ebook", []string{"rum"})
			if err != nil {
				t.Fatal(err)
			}
			id := rows[0].ID
			files := NewBookFileRepo(database)
			path := "/lib/reopen-" + mode + ".epub"
			if err := files.Add(ctx, book.ID, "ebook", path); err != nil {
				t.Fatal(err)
			}
			if err := files.SetVariant(ctx, path, &id, "rum", "verified"); err != nil {
				t.Fatal(err)
			}
			if err := variants.SetStatus(ctx, id, models.VariantImported); err != nil {
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
					t.Fatalf("reclaim = %#v, %v", result, err)
				}
			}
			got, err := variants.ListByBook(ctx, book.ID)
			if err != nil || len(got) != 1 || got[0].Status != models.VariantWanted {
				t.Fatalf("former variant after %s = %#v, %v; want wanted", mode, got, err)
			}
			wanted, err := variants.ListWanted(ctx)
			if err != nil || len(wanted) != 1 || wanted[0].ID != id {
				t.Fatalf("wanted after %s = %#v, %v", mode, wanted, err)
			}
		})
	}
}

// Another verified file keeps the old variant imported. An active grab keeps
// it in progress even when the moved file was the only imported copy.
func TestBookFileOwnershipChangeKeepsOtherVariantClaims(t *testing.T) {
	for _, remaining := range []string{"verified file", "active download"} {
		t.Run(remaining, func(t *testing.T) {
			database, author, book := openTestDB(t)
			ctx := context.Background()
			other := &models.Book{ForeignID: "keep-file-" + remaining, AuthorID: author.ID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
			if err := NewBookRepo(database).Create(ctx, other); err != nil {
				t.Fatal(err)
			}
			variants := NewBookVariantRepo(database)
			rows, err := variants.EnsureWanted(ctx, book.ID, "ebook", []string{"rum"})
			if err != nil {
				t.Fatal(err)
			}
			id := rows[0].ID
			files := NewBookFileRepo(database)
			const path = "/lib/keep-first.epub"
			if err := files.Add(ctx, book.ID, "ebook", path); err != nil {
				t.Fatal(err)
			}
			if err := files.SetVariant(ctx, path, &id, "rum", "verified"); err != nil {
				t.Fatal(err)
			}
			wantStatus := models.VariantImported
			switch remaining {
			case "verified file":
				if err := files.Add(ctx, book.ID, "ebook", "/lib/keep-second.epub"); err != nil {
					t.Fatal(err)
				}
				if err := files.SetVariant(ctx, "/lib/keep-second.epub", &id, "rum", "verified"); err != nil {
					t.Fatal(err)
				}
			case "active download":
				d := &models.Download{GUID: "still-loading", BookID: &book.ID, VariantID: &id, Title: "Loading", Status: models.StateGrabbed, Protocol: "usenet"}
				if err := NewDownloadRepo(database).Create(ctx, d); err != nil {
					t.Fatal(err)
				}
				wantStatus = models.VariantDownloading
			}
			if err := variants.SetStatus(ctx, id, models.VariantImported); err != nil {
				t.Fatal(err)
			}
			if _, err := files.MoveToBook(ctx, other.ID, "ebook", path); err != nil {
				t.Fatal(err)
			}
			got, err := variants.ListByBook(ctx, book.ID)
			if err != nil || len(got) != 1 || got[0].Status != wantStatus {
				t.Fatalf("variant with remaining %s = %#v, %v; want %s", remaining, got, err, wantStatus)
			}
		})
	}
}

// A download moving to another book must not leave the former variant hidden
// from the wanted sweep when it was the only live claim.
func TestDownloadBookReassignmentReopensUnclaimedVariant(t *testing.T) {
	for _, tc := range []struct {
		name     string
		download models.DownloadState
		variant  string
	}{
		{"active grab", models.StateGrabbed, models.VariantDownloading},
		{"historical import", models.StateImported, models.VariantImported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, author, book := openTestDB(t)
			ctx := context.Background()
			other := &models.Book{ForeignID: "reopen-download-" + tc.name, AuthorID: author.ID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
			if err := NewBookRepo(database).Create(ctx, other); err != nil {
				t.Fatal(err)
			}
			variants := NewBookVariantRepo(database)
			rows, err := variants.EnsureWanted(ctx, book.ID, "ebook", []string{"rum"})
			if err != nil {
				t.Fatal(err)
			}
			id := rows[0].ID
			downloads := NewDownloadRepo(database)
			d := &models.Download{GUID: "reopen-download-" + tc.name, BookID: &book.ID, VariantID: &id, Title: "Previous", Status: tc.download, Protocol: "usenet"}
			if err := downloads.Create(ctx, d); err != nil {
				t.Fatal(err)
			}
			if err := variants.SetStatus(ctx, id, tc.variant); err != nil {
				t.Fatal(err)
			}
			if err := downloads.SetBookID(ctx, d.ID, other.ID); err != nil {
				t.Fatal(err)
			}
			got, err := variants.ListByBook(ctx, book.ID)
			if err != nil || len(got) != 1 || got[0].Status != models.VariantWanted {
				t.Fatalf("former variant after %s = %#v, %v; want wanted", tc.name, got, err)
			}
			wanted, err := variants.ListWanted(ctx)
			if err != nil || len(wanted) != 1 || wanted[0].ID != id {
				t.Fatalf("wanted after %s = %#v, %v", tc.name, wanted, err)
			}
		})
	}
}

func TestDownloadBookReassignmentKeepsOtherVariantClaims(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wantStatus string
	}{
		{"verified file", models.VariantImported},
		{"active download", models.VariantDownloading},
		{"failed download", models.VariantWanted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, author, book := openTestDB(t)
			ctx := context.Background()
			other := &models.Book{ForeignID: "keep-download-" + tc.name, AuthorID: author.ID, Title: "Other", SortTitle: "Other", MediaType: models.MediaTypeEbook, Status: models.BookStatusWanted}
			if err := NewBookRepo(database).Create(ctx, other); err != nil {
				t.Fatal(err)
			}
			variants := NewBookVariantRepo(database)
			rows, err := variants.EnsureWanted(ctx, book.ID, "ebook", []string{"rum"})
			if err != nil {
				t.Fatal(err)
			}
			id := rows[0].ID
			downloads := NewDownloadRepo(database)
			d := &models.Download{GUID: "moving-" + tc.name, BookID: &book.ID, VariantID: &id, Title: "Moving", Status: models.StateGrabbed, Protocol: "usenet"}
			if err := downloads.Create(ctx, d); err != nil {
				t.Fatal(err)
			}
			switch tc.name {
			case "verified file":
				files := NewBookFileRepo(database)
				if err := files.Add(ctx, book.ID, "ebook", "/lib/keep-download.epub"); err != nil {
					t.Fatal(err)
				}
				if err := files.SetVariant(ctx, "/lib/keep-download.epub", &id, "rum", "verified"); err != nil {
					t.Fatal(err)
				}
			case "active download", "failed download":
				state := models.StateGrabbed
				if tc.name == "failed download" {
					state = models.StateFailed
				}
				second := &models.Download{GUID: "remaining-" + tc.name, BookID: &book.ID, VariantID: &id, Title: "Remaining", Status: state, Protocol: "usenet"}
				if err := downloads.Create(ctx, second); err != nil {
					t.Fatal(err)
				}
			}
			if err := variants.SetStatus(ctx, id, models.VariantDownloading); err != nil {
				t.Fatal(err)
			}
			if err := downloads.SetBookID(ctx, d.ID, other.ID); err != nil {
				t.Fatal(err)
			}
			got, err := variants.ListByBook(ctx, book.ID)
			if err != nil || len(got) != 1 || got[0].Status != tc.wantStatus {
				t.Fatalf("variant with remaining %s = %#v, %v; want %s", tc.name, got, err, tc.wantStatus)
			}
		})
	}
}

func TestBookFileRepoSetVariantReopensReplacedClaim(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	variants := NewBookVariantRepo(database)
	rows, err := variants.EnsureWanted(ctx, book.ID, "ebook", []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	files := NewBookFileRepo(database)
	const path = "/lib/changed-language.epub"
	if err := files.Add(ctx, book.ID, "ebook", path); err != nil {
		t.Fatal(err)
	}
	if err := files.SetVariant(ctx, path, &rows[0].ID, "eng", "verified"); err != nil {
		t.Fatal(err)
	}
	if err := variants.SetStatus(ctx, rows[0].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	if err := files.SetVariant(ctx, path, &rows[1].ID, "rum", "verified"); err != nil {
		t.Fatal(err)
	}
	got, err := variants.ListByBook(ctx, book.ID)
	if err != nil || len(got) != 2 || got[0].Status != models.VariantWanted {
		t.Fatalf("former file variant = %#v, %v; want English wanted", got, err)
	}
}

func TestDownloadRegrabReopensPreviousVariant(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	variants := NewBookVariantRepo(database)
	rows, err := variants.EnsureWanted(ctx, book.ID, "ebook", []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	downloads := NewDownloadRepo(database)
	d := &models.Download{GUID: "changed-regrab-language", BookID: &book.ID, VariantID: &rows[0].ID, Title: "Old", Status: models.StateFailed, Protocol: "usenet"}
	if err := downloads.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := variants.SetStatus(ctx, rows[0].ID, models.VariantDownloading); err != nil {
		t.Fatal(err)
	}
	d.VariantID = &rows[1].ID
	claimed, err := downloads.RetryFailed(ctx, d)
	if err != nil || !claimed {
		t.Fatalf("RetryFailed = %t, %v", claimed, err)
	}
	got, err := variants.ListByBook(ctx, book.ID)
	if err != nil || len(got) != 2 || got[0].Status != models.VariantWanted {
		t.Fatalf("former download variant = %#v, %v; want English wanted", got, err)
	}
}
