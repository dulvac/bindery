//go:build !windows

package importer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// A failed merge cannot safely undo a public name: another writer may have
// replaced it after we linked it. The original source must still be seeded.
func TestHardlinkDirMergeStrict_ReplacementAtFailureSurvives(t *testing.T) {
	src := mustDir(t, filepath.Join(t.TempDir(), "source"))
	first := mustFile(t, filepath.Join(src, "01.m4b"), "seed one")
	second := mustFile(t, filepath.Join(src, "02.m4b"), "seed two")
	dst := mustDir(t, filepath.Join(t.TempDir(), "library"))
	mustFile(t, filepath.Join(dst, "book.epub"), "existing ebook")
	originalLink := osLink
	links := 0
	var firstPublic, failedPublic string
	osLink = func(from, to string) error {
		links++
		if links == 2 {
			failedPublic = to
			// os.Root's ReadDir uses filesystem order, not name order.
			// Replace whichever public name the first link actually placed.
			if err := os.Remove(firstPublic); err != nil {
				t.Fatal(err)
			}
			mustFile(t, firstPublic, "someone else's replacement")
			mustFile(t, filepath.Join(dst, "neighbor.txt"), "someone else's addition")
			return &os.LinkError{Op: "link", Old: from, New: to, Err: syscall.EXDEV}
		}
		firstPublic = to
		return originalLink(from, to)
	}
	t.Cleanup(func() { osLink = originalLink })

	_, err := HardlinkDirMergeStrict(src, dst)
	if !errors.Is(err, syscall.EXDEV) {
		t.Errorf("expected EXDEV, got %v", err)
	}
	for path, want := range map[string]string{
		filepath.Join(dst, "book.epub"):    "existing ebook",
		firstPublic:                        "someone else's replacement",
		filepath.Join(dst, "neighbor.txt"): "someone else's addition",
		first:                              "seed one",
		second:                             "seed two",
	} {
		got, readErr := os.ReadFile(path)
		if readErr != nil || string(got) != want {
			t.Errorf("%s: %q, %v; want %q", path, got, readErr, want)
		}
	}
	if _, statErr := os.Stat(failedPublic); !os.IsNotExist(statErr) {
		t.Errorf("failed link placed a copy: %v", statErr)
	}
	if err == nil || !strings.Contains(err.Error(), "reconcile") || !strings.Contains(err.Error(), dst) {
		t.Errorf("expected explicit reconciliation path, got %v", err)
	}
}

func TestHardlinkDirStrict_FailurePreservesConcurrentPublicChanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace bool
	}{
		{name: "addition"},
		{name: "replacement directory", replace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := mustDir(t, filepath.Join(t.TempDir(), "source"))
			first := mustFile(t, filepath.Join(src, "01.m4b"), "seed one")
			second := mustFile(t, filepath.Join(src, "02.m4b"), "seed two")
			dst := filepath.Join(t.TempDir(), "library")
			originalLink := osLink
			links := 0
			var firstPublic, firstSource, failedPublic string
			osLink = func(from, to string) error {
				links++
				if links == 2 {
					failedPublic = to
					if tc.replace {
						if err := os.Rename(dst, dst+"-former"); err != nil {
							t.Fatal(err)
						}
					}
					mustFile(t, filepath.Join(dst, "neighbor.txt"), "someone else's data")
					return &os.LinkError{Op: "link", Old: from, New: to, Err: syscall.EXDEV}
				}
				firstSource, firstPublic = from, to
				return originalLink(from, to)
			}
			t.Cleanup(func() { osLink = originalLink })

			placeErr := HardlinkDirStrict(src, dst)
			if !errors.Is(placeErr, syscall.EXDEV) {
				t.Errorf("expected EXDEV, got %v", placeErr)
			}
			if got, err := os.ReadFile(filepath.Join(dst, "neighbor.txt")); err != nil || string(got) != "someone else's data" {
				t.Errorf("concurrent public data removed: %q, %v", got, err)
			}
			linkedPath := firstPublic
			if tc.replace {
				linkedPath = filepath.Join(dst+"-former", filepath.Base(firstPublic))
			}
			linked, err := os.Stat(linkedPath)
			original, srcErr := os.Stat(firstSource)
			if err != nil || srcErr != nil || !os.SameFile(linked, original) {
				t.Errorf("partial hardlink/source lost: %v, %v", err, srcErr)
			}
			for path, want := range map[string]string{first: "seed one", second: "seed two"} {
				if got, err := os.ReadFile(path); err != nil || string(got) != want {
					t.Errorf("seed %s changed: %q, %v", path, got, err)
				}
			}
			if _, err := os.Stat(failedPublic); !os.IsNotExist(err) {
				t.Errorf("failed link placed a copy: %v", err)
			}
			if placeErr == nil || !strings.Contains(placeErr.Error(), "reconcile") || !strings.Contains(placeErr.Error(), dst) {
				t.Errorf("expected explicit reconciliation path, got %v", placeErr)
			}
		})
	}
}

// The scanner's per-file path has its own rollback outside the directory
// helper. It must not unlink a replacement at a public name either.
func TestLegacyQbitPerFileFailurePreservesPublicReplacement(t *testing.T) {
	s, dl, downloads, books, ctx := dataLossFixture(t, t.TempDir(), "move")
	book, err := books.GetByID(ctx, *dl.BookID)
	if err != nil {
		t.Fatal(err)
	}
	book.MediaType = models.MediaTypeAudiobook
	if err := books.Update(ctx, book); err != nil {
		t.Fatal(err)
	}
	srcRoot := t.TempDir()
	first := mustFile(t, filepath.Join(srcRoot, "Part A", "one.mp3"), "seed one")
	second := mustFile(t, filepath.Join(srcRoot, "Part B", "two.mp3"), "seed two")
	originalLink := osLink
	links := 0
	var firstPublic, failedPublic string
	osLink = func(from, to string) error {
		links++
		if links == 2 {
			failedPublic = to
			if err := os.Remove(firstPublic); err != nil {
				t.Fatal(err)
			}
			mustFile(t, firstPublic, "other writer")
			return &os.LinkError{Op: "link", Old: from, New: to, Err: syscall.EXDEV}
		}
		firstPublic = to
		return originalLink(from, to)
	}
	t.Cleanup(func() { osLink = originalLink })

	s.tryImportInternal(ctx, dl, srcRoot, "qbittorrent", "fake-hash", "", nil, []string{first, second})
	got, err := downloads.GetByID(ctx, dl.ID)
	if err != nil || got.Status != models.StateImportBlocked || !strings.Contains(got.ErrorMessage, "reconcile") {
		t.Errorf("blocked import must ask for reconciliation: %+v, %v", got, err)
	}
	if firstPublic == "" || failedPublic == "" {
		t.Fatalf("expected two placement attempts, got %d", links)
	}
	if b, err := os.ReadFile(firstPublic); err != nil || string(b) != "other writer" {
		t.Errorf("scanner deleted public replacement: %q, %v", b, err)
	}
	if _, err := os.Stat(failedPublic); !os.IsNotExist(err) {
		t.Errorf("failed link produced a copy: %v", err)
	}
	for path, want := range map[string]string{first: "seed one", second: "seed two"} {
		if b, err := os.ReadFile(path); err != nil || string(b) != want {
			t.Errorf("source %s changed: %q, %v", path, b, err)
		}
	}
}

// A successful strict placement may still fail when recording the book path.
// That DB failure must not RemoveAll a folder another writer has added to.
func TestLegacyQbitAudioRecordFailurePreservesPublicAddition(t *testing.T) {
	s, book, downloads, books, _, database, ctx := sharedFormatFixture(t, t.TempDir())
	book.MediaType = models.MediaTypeAudiobook
	if err := books.Update(ctx, book); err != nil {
		t.Fatal(err)
	}
	src := mustDir(t, filepath.Join(t.TempDir(), "recording"))
	first := mustFile(t, filepath.Join(src, "01.mp3"), "seed one")
	second := mustFile(t, filepath.Join(src, "02.mp3"), "seed two")
	dl := &models.Download{GUID: "qbit-record-fails", Title: book.Title, BookID: &book.ID, Status: models.StateCompleted, Protocol: "torrent"}
	if err := downloads.Create(ctx, dl); err != nil {
		t.Fatal(err)
	}
	originalLink := osLink
	var publicDir string
	osLink = func(from, to string) error {
		if err := originalLink(from, to); err != nil {
			return err
		}
		if publicDir == "" {
			publicDir = filepath.Dir(to)
			mustFile(t, filepath.Join(publicDir, "neighbor.txt"), "other writer")
			if _, err := database.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	t.Cleanup(func() {
		osLink = originalLink
		_, _ = database.ExecContext(ctx, "PRAGMA query_only = OFF")
	})

	s.tryImportInternal(withTorrentOwnFolder(ctx, true), dl, src, "qbittorrent", "fake-hash", "", nil, []string{first, second})
	if publicDir == "" {
		t.Fatal("strict audiobook placement never started")
	}
	if got, err := os.ReadFile(filepath.Join(publicDir, "neighbor.txt")); err != nil || string(got) != "other writer" {
		t.Errorf("record failure deleted another writer's data: %q, %v", got, err)
	}
	for path, want := range map[string]string{first: "seed one", second: "seed two"} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("seed %s changed: %q, %v", path, got, err)
		}
	}
}

func TestFlattenAudiobookDirStrict_EXDEVLeavesSourceAndPublicLinks(t *testing.T) {
	for _, failName := range []string{"Part 002.mp3", "cover.jpg"} {
		t.Run(failName, func(t *testing.T) {
			src := buildMultiDiscTree(t)
			dst := filepath.Join(t.TempDir(), "library")
			originalLink := osLink
			osLink = func(from, to string) error {
				if filepath.Base(to) == failName {
					return &os.LinkError{Op: "link", Old: from, New: to, Err: syscall.EXDEV}
				}
				return originalLink(from, to)
			}
			t.Cleanup(func() { osLink = originalLink })

			placeErr := flattenAudiobookDir(context.Background(), "hardlink-strict", src, dst)
			if !errors.Is(placeErr, syscall.EXDEV) {
				t.Errorf("expected EXDEV, got %v", placeErr)
			}
			original, err := os.Stat(filepath.Join(src, "Disc 1", "Track 01.mp3"))
			if err != nil {
				t.Fatal(err)
			}
			linked, err := os.Stat(filepath.Join(dst, "Part 001.mp3"))
			if err != nil || !os.SameFile(original, linked) {
				t.Errorf("partial public link or seeded source lost: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dst, failName)); !os.IsNotExist(err) {
				t.Errorf("failed %s link produced a copy: %v", failName, err)
			}
			for path, want := range map[string]string{
				filepath.Join(src, "Disc 1", "Track 01.mp3"): "d1t1",
				filepath.Join(src, "Disc 1", "Track 02.mp3"): "d1t2",
				filepath.Join(src, "cover.jpg"):              "img",
			} {
				got, readErr := os.ReadFile(path)
				if readErr != nil || string(got) != want {
					t.Errorf("seed %s changed: %q, %v", path, got, readErr)
				}
			}
			if placeErr == nil || !strings.Contains(placeErr.Error(), "reconcile") || !strings.Contains(placeErr.Error(), dst) {
				t.Errorf("expected explicit reconciliation path, got %v", placeErr)
			}
		})
	}
}
