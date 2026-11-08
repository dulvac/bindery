package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

func TestReorganizeVariantEbooksKeepDistinctLanguagePaths(t *testing.T) {
	env, libraryDir, _, ctx := reorgFixture(t)
	book := env.seed(t, ctx, "Jane Doe", "My Book")
	variants, err := env.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range variants {
		path := filepath.Join(libraryDir, "unsorted", v.Language+".epub")
		writeFileAt(t, path)
		if err := env.books.TrackVariantFile(ctx, book.ID, v.ID, models.MediaTypeEbook, path, v.Language, "verified"); err != nil {
			t.Fatal(err)
		}
	}
	moves, err := env.s.PreviewReorganizeBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(moves) != 2 {
		t.Fatalf("preview = %+v, want two variants", moves)
	}
	ids := make([]int64, 0, len(moves))
	seen := map[string]bool{}
	for _, m := range moves {
		if m.Status != ReorgStatusMove || seen[m.Proposed] ||
			!strings.Contains(m.Proposed, " [eng].epub") && !strings.Contains(m.Proposed, " [rum].epub") {
			t.Fatalf("unsafe variant preview: %+v", moves)
		}
		seen[m.Proposed] = true
		ids = append(ids, m.FileID)
	}
	for _, result := range env.s.ApplyReorganize(ctx, ids) {
		if result.Status != ReorgStatusMoved {
			t.Fatalf("apply = %+v", result)
		}
		if _, err := os.Stat(result.Proposed); err != nil {
			t.Fatal(err)
		}
	}
	files, err := env.books.ListBookFiles(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %+v", files)
	}
	for _, f := range files {
		if f.VariantID == nil || f.Language == "" || !strings.Contains(f.Path, " ["+f.Language+"].epub") {
			t.Fatalf("lost variant association after rename: %+v", f)
		}
	}
	second, err := env.s.PreviewReorganizeBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range second {
		if m.Status != ReorgStatusNoop {
			t.Fatalf("second preview = %+v", second)
		}
	}
}

func TestReorganizeVariantAudiobookFoldersKeepLanguagePaths(t *testing.T) {
	env, _, audiobookDir, ctx := reorgFixture(t)
	book := env.seed(t, ctx, "Jane Doe", "My Book")
	variants, err := env.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeAudiobook, []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range variants {
		path := filepath.Join(audiobookDir, "unsorted", v.Language)
		writeFileAt(t, filepath.Join(path, "part1.mp3"))
		if err := env.books.TrackVariantFile(ctx, book.ID, v.ID, models.MediaTypeAudiobook, path, v.Language, "verified"); err != nil {
			t.Fatal(err)
		}
	}
	moves, err := env.s.PreviewReorganizeBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(moves) != 2 {
		t.Fatalf("preview = %+v, want two variants", moves)
	}
	ids := make([]int64, 0, len(moves))
	for _, m := range moves {
		if m.Status != ReorgStatusMove || !strings.HasSuffix(m.Proposed, " [eng]") && !strings.HasSuffix(m.Proposed, " [rum]") {
			t.Fatalf("unsafe variant audio preview: %+v", moves)
		}
		ids = append(ids, m.FileID)
	}
	for _, result := range env.s.ApplyReorganize(ctx, ids) {
		if result.Status != ReorgStatusMoved {
			t.Fatalf("apply = %+v", result)
		}
		if _, err := os.Stat(filepath.Join(result.Proposed, "part1.mp3")); err != nil {
			t.Fatal(err)
		}
	}
	second, err := env.s.PreviewReorganizeBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range second {
		if m.Status != ReorgStatusNoop {
			t.Fatalf("second preview = %+v", second)
		}
	}
}

func TestReorganizeVariantMissingLanguageFailsClosed(t *testing.T) {
	env, libraryDir, _, ctx := reorgFixture(t)
	book := env.seed(t, ctx, "Jane Doe", "My Book")
	variants, err := env.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(libraryDir, "unsorted", "unknown.epub")
	writeFileAt(t, path)
	if err := env.books.TrackVariantFile(ctx, book.ID, variants[0].ID, models.MediaTypeEbook, path, "", "unverified"); err != nil {
		t.Fatal(err)
	}
	moves, err := env.s.PreviewReorganizeBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(moves) != 1 || moves[0].Status != ReorgStatusError {
		t.Fatalf("preview = %+v, want error", moves)
	}
	if got := env.s.ApplyReorganize(ctx, []int64{moves[0].FileID}); len(got) != 1 || got[0].Status != ReorgStatusError {
		t.Fatalf("apply = %+v, want no move", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestReorganizeVariantRejectsDetectedLanguageThatDiffersFromRequestedSlot(t *testing.T) {
	env, libraryDir, _, ctx := reorgFixture(t)
	book := env.seed(t, ctx, "Jane Doe", "My Book")
	variants, err := env.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(libraryDir, "unsorted", "override [eng].epub")
	writeFileAt(t, path)
	if err := env.books.TrackVariantFile(ctx, book.ID, variants[0].ID, models.MediaTypeEbook, path, "rum", "unverified"); err != nil {
		t.Fatal(err)
	}
	moves, err := env.s.PreviewReorganizeBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(moves) != 1 || moves[0].Status != ReorgStatusError {
		t.Fatalf("preview = %+v, want error on mismatched slot", moves)
	}
	if results := env.s.ApplyReorganize(ctx, []int64{moves[0].FileID}); len(results) != 1 || results[0].Status != ReorgStatusError {
		t.Fatalf("apply = %+v, want no move", results)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestReorganizeVariantAudiobookNeverLeavesRootForEmptyTemplate(t *testing.T) {
	env, _, audiobookDir, ctx := reorgFixture(t)
	book := env.seed(t, ctx, "Jane Doe", "My Book")
	variants, err := env.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeAudiobook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(audiobookDir, "unsorted", "old-folder")
	writeFileAt(t, filepath.Join(oldDir, "track.mp3"))
	if err := env.books.TrackVariantFile(ctx, book.ID, variants[0].ID, models.MediaTypeAudiobook, oldDir, "eng", "verified"); err != nil {
		t.Fatal(err)
	}
	if err := env.s.settings.Set(ctx, "naming_template_audiobook", "{Series}"); err != nil {
		t.Fatal(err)
	}
	moves, err := env.s.PreviewReorganizeBook(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(moves) != 1 || moves[0].Status != ReorgStatusError {
		t.Fatalf("preview = %+v, want outside-root error", moves)
	}
	if results := env.s.ApplyReorganize(ctx, []int64{moves[0].FileID}); len(results) != 1 || results[0].Status != ReorgStatusError {
		t.Fatalf("apply = %+v, want no move", results)
	}
	if _, err := os.Stat(filepath.Join(oldDir, "track.mp3")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(audiobookDir + " [eng]"); !os.IsNotExist(err) {
		t.Fatalf("outside-root destination exists: %v", err)
	}
}

func TestReorganizeVariantAudioFolderCollisionThenRetryKeepsSeedLink(t *testing.T) {
	env, _, audiobookDir, ctx := reorgFixture(t)
	book := env.seed(t, ctx, "Jane Doe", "My Book")
	variants, err := env.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeAudiobook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(t.TempDir(), "seed.mp3")
	writeFileAt(t, seed)
	oldDir := filepath.Join(audiobookDir, "unsorted", "eng")
	if err := os.MkdirAll(oldDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(seed, filepath.Join(oldDir, "track.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := env.books.TrackVariantFile(ctx, book.ID, variants[0].ID, models.MediaTypeAudiobook, oldDir, "eng", "verified"); err != nil {
		t.Fatal(err)
	}
	moves, err := env.s.PreviewReorganizeBook(ctx, book.ID)
	if err != nil || len(moves) != 1 || moves[0].Status != ReorgStatusMove {
		t.Fatalf("preview = %+v %v", moves, err)
	}
	dest := moves[0].Proposed
	if err := os.MkdirAll(dest, 0o750); err != nil {
		t.Fatal(err)
	}
	files, err := env.books.ListBookFiles(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.s.moveTrackedFile(ctx, files[0], dest); err == nil {
		t.Fatal("move replaced an empty destination folder")
	}
	if _, err := os.Stat(filepath.Join(oldDir, "track.mp3")); err != nil {
		t.Fatalf("source folder moved on collision: %v", err)
	}
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	results := env.s.ApplyReorganize(ctx, []int64{files[0].ID})
	if len(results) != 1 || results[0].Status != ReorgStatusMoved {
		t.Fatalf("retry = %+v", results)
	}
	seedInfo, err := os.Stat(seed)
	if err != nil {
		t.Fatal(err)
	}
	trackInfo, err := os.Stat(filepath.Join(dest, "track.mp3"))
	if err != nil || !os.SameFile(seedInfo, trackInfo) {
		t.Fatalf("seed hardlink changed: %v", err)
	}
	tracked, err := env.books.ListBookFiles(ctx, book.ID)
	if err != nil || tracked[0].Path != dest || tracked[0].VariantID == nil {
		t.Fatalf("tracked variant after retry = %+v %v", tracked, err)
	}
}

func TestReorganizeMoveDoesNotReplaceNewDestination(t *testing.T) {
	env, libraryDir, _, ctx := reorgFixture(t)
	book := env.seed(t, ctx, "Jane Doe", "My Book")
	oldPath := filepath.Join(libraryDir, "unsorted", "book.epub")
	writeFileAt(t, oldPath)
	if err := env.books.AddBookFile(ctx, book.ID, models.MediaTypeEbook, oldPath); err != nil {
		t.Fatal(err)
	}
	moves, err := env.s.PreviewReorganizeBook(ctx, book.ID)
	if err != nil || len(moves) != 1 || moves[0].Status != ReorgStatusMove {
		t.Fatalf("preview = %+v %v", moves, err)
	}
	dest := moves[0].Proposed
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("new occupant"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := env.books.ListBookFiles(ctx, book.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.s.moveTrackedFile(ctx, files[0], dest); err == nil {
		t.Fatal("move replaced a new destination")
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "new occupant" {
		t.Fatalf("destination changed to %q: %v", got, err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("source moved after collision: %v", err)
	}
	tracked, err := env.books.ListBookFiles(ctx, book.ID)
	if err != nil || tracked[0].Path != oldPath {
		t.Fatalf("index changed after collision: %+v %v", tracked, err)
	}
}
