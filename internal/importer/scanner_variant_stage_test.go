package importer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

func variantEbookTarget(t *testing.T, f *variantReviewFixture, source string) string {
	t.Helper()
	author, err := db.NewAuthorRepo(f.database).GetByID(f.ctx, f.book.AuthorID)
	if err != nil {
		t.Fatal(err)
	}
	base, err := f.scanner.destRenamer(f.ctx).DestPath(f.library, author, f.book, "", "", source)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := VariantDestination(base, f.variant.Language, false)
	if err != nil {
		t.Fatal(err)
	}
	return dest
}

func variantAudioTarget(t *testing.T, f *variantReviewFixture) string {
	t.Helper()
	author, err := db.NewAuthorRepo(f.database).GetByID(f.ctx, f.book.AuthorID)
	if err != nil {
		t.Fatal(err)
	}
	base, err := f.scanner.destRenamer(f.ctx).AudiobookDestDir(f.library, author, f.book, "", "")
	if err != nil {
		t.Fatal(err)
	}
	dest, err := VariantDestination(base, f.variant.Language, true)
	if err != nil {
		t.Fatal(err)
	}
	return dest
}

func variantAudioSources(t *testing.T, f *variantReviewFixture) (string, []string) {
	t.Helper()
	folder := filepath.Join(f.sources, "recording")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	files := []string{filepath.Join(folder, "01.m4b"), filepath.Join(folder, "02.m4b")}
	for _, p := range files {
		if err := os.WriteFile(p, mp4File(atom("\xa9alb", dataAtom(1, []byte("Dune"))), atom("\xa9ART", dataAtom(1, []byte("Frank Herbert")))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return folder, files
}

func variantBlocked(t *testing.T, f *variantReviewFixture, dl *models.Download, fragment string) {
	t.Helper()
	got, err := f.downloads.GetByID(f.ctx, dl.ID)
	if err != nil || got.Status != models.StateImportBlocked || !strings.Contains(got.ErrorMessage, fragment) {
		t.Fatalf("expected blocked %q: %+v %v", fragment, got, err)
	}
}

func TestVariantCollision_ExistingEbookAndConcurrentDirectoryAreNeverReplaced(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(map[bool]string{true: "concurrent directory", false: "existing file"}[concurrent], func(t *testing.T) {
			f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
			source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
			dest := variantEbookTarget(t, f, source)
			if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
				t.Fatal(err)
			}
			if concurrent {
				variantPromotionHook = func() {
					if err := os.Mkdir(dest, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() { variantPromotionHook = nil })
			} else if err := os.WriteFile(dest, []byte("original entry"), 0o600); err != nil {
				t.Fatal(err)
			}
			dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
			f.importSource(t, dl, source)
			variantBlocked(t, f, dl, "no-replace")
			variantImportedFiles(t, f, 0)
			if info, err := os.Lstat(dest); err != nil || info.IsDir() != concurrent {
				t.Fatalf("public entry changed: %v %v", info, err)
			}
			if !concurrent {
				if b, err := os.ReadFile(dest); err != nil || string(b) != "original entry" {
					t.Fatalf("file overwritten: %q %v", b, err)
				}
			}
			if _, err := os.Lstat(source); err != nil {
				t.Fatalf("seed removed: %v", err)
			}
		})
	}
}

func TestVariantStage_UnreadableAudioSourceFailsWithoutChangingSeed(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
	folder, files := variantAudioSources(t, f)
	for _, source := range files {
		if err := os.Chmod(source, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(files[1], 0o600); err != nil {
		t.Fatal(err)
	}
	dl := f.addDownload(t, "Dune by Frank Herbert [ENG / M4B]")
	f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "hash", "", nil, files)
	variantBlocked(t, f, dl, "not readable by ABS")
	if rows, err := f.books.ListFiles(f.ctx, f.book.ID); err != nil || len(rows) != 0 {
		t.Fatalf("unreadable audio tracked: %+v %v", rows, err)
	}
	if _, err := os.Lstat(variantAudioTarget(t, f)); !os.IsNotExist(err) {
		t.Fatalf("unreadable audio published: %v", err)
	}
	if info, err := os.Stat(files[1]); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("torrent source mode changed: %v %v", info, err)
	}
}

func TestVariantStage_AudioDirectoryAndDiscReadableBySeparateABSUID(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
	folder, files := variantAudioSources(t, f)
	disc := filepath.Join(folder, "Disc 1")
	if err := os.Mkdir(disc, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(files[1], filepath.Join(disc, "02.m4b")); err != nil {
		t.Fatal(err)
	}
	files[1] = filepath.Join(disc, "02.m4b")
	for _, source := range files {
		if err := os.Chmod(source, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dl := f.addDownload(t, "Dune by Frank Herbert [ENG / M4B]")
	f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "hash", "", nil, files)
	variantImportedDownload(t, f, dl.ID)
	dest := variantAudioTarget(t, f)
	for _, dir := range []string{dest, filepath.Join(dest, "Disc 1")} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0o005 != 0o005 {
			t.Fatalf("simulated ABS UID cannot read/traverse %s: %v %v", dir, info, err)
		}
	}
	for _, source := range files {
		rel, _ := filepath.Rel(folder, source)
		linked, err := os.Stat(filepath.Join(dest, rel))
		if err != nil || linked.Mode().Perm()&0o004 == 0 || !sameFilePath(t, source, linked) {
			t.Fatalf("simulated ABS UID cannot read unchanged track %s: %v %v", rel, linked, err)
		}
	}
	stageInfo, err := os.Stat(filepath.Join(filepath.Dir(f.library), ".bindery-staging"))
	if err != nil || stageInfo.Mode().Perm() != 0o700 {
		t.Fatalf("stage root lost private mode: %v %v", stageInfo, err)
	}
}

func TestVariantCollision_AudioFolderNoReplaceAndAllOrNothing(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing folder", true: "second track fails"}[failSecond], func(t *testing.T) {
			f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
			folder, files := variantAudioSources(t, f)
			dest := variantAudioTarget(t, f)
			if failSecond {
				original := variantStageLink
				variantStageLink = func(src string, parentFD int, name string) error {
					if src == files[1] {
						return errors.New("test second track failure")
					}
					return original(src, parentFD, name)
				}
				t.Cleanup(func() { variantStageLink = original })
			} else {
				if err := os.MkdirAll(dest, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dest, "keep"), []byte("preexisting"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			dl := f.addDownload(t, "Dune by Frank Herbert [ENG / M4B]")
			f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "hash", "", nil, files)
			variantBlocked(t, f, dl, map[bool]string{true: "second track failure", false: "no-replace"}[failSecond])
			variantImportedFiles(t, f, 0)
			if failSecond {
				if _, err := os.Lstat(dest); !os.IsNotExist(err) {
					t.Fatalf("partial public audio: %v", err)
				}
			} else {
				if b, err := os.ReadFile(filepath.Join(dest, "keep")); err != nil || string(b) != "preexisting" {
					t.Fatalf("old folder changed: %q %v", b, err)
				}
			}
			for _, p := range files {
				if _, err := os.Lstat(p); err != nil {
					t.Fatalf("seed removed: %v", err)
				}
			}
		})
	}
}

func TestVariantCollision_ConcurrentAudioDirectoryCannotBeReplaced(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
	folder, files := variantAudioSources(t, f)
	dest := variantAudioTarget(t, f)
	variantPromotionHook = func() {
		if err := os.MkdirAll(dest, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, "keep"), []byte("concurrent owner"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { variantPromotionHook = nil })
	dl := f.addDownload(t, "Dune by Frank Herbert [ENG / M4B]")
	f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "hash", "", nil, files)
	variantBlocked(t, f, dl, "no-replace")
	variantImportedFiles(t, f, 0)
	if b, err := os.ReadFile(filepath.Join(dest, "keep")); err != nil || string(b) != "concurrent owner" {
		t.Fatalf("concurrent folder replaced: %q %v", b, err)
	}
	for _, p := range files {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("source removed: %v", err)
		}
	}
}

func TestVariantStage_SourceChangedBetweenReviewAndPromotionCannotPublish(t *testing.T) {
	for _, swap := range []bool{false, true} {
		t.Run(map[bool]string{true: "inode replaced before staging", false: "mtime changed after staging"}[swap], func(t *testing.T) {
			f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
			source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
			dest := variantEbookTarget(t, f, source)
			original := variantStageLink
			if swap {
				variantStageLink = func(src string, parentFD int, name string) error {
					if src == source {
						if err := os.Rename(src, src+".old"); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(src, []byte("replacement"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					return original(src, parentFD, name)
				}
				t.Cleanup(func() { variantStageLink = original })
			} else {
				variantPromotionHook = func() {
					later := time.Now().Add(2 * time.Hour)
					if err := os.Chtimes(source, later, later); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() { variantPromotionHook = nil })
			}
			dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
			f.importSource(t, dl, source)
			variantBlocked(t, f, dl, "variant source")
			variantImportedFiles(t, f, 0)
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatalf("changed source published: %v", err)
			}
		})
	}
}

func TestVariantStage_RenamedRootAndSymlinkCannotRedirectStagingOrCleanup(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	dest := variantEbookTarget(t, f, source)
	stage := filepath.Join(filepath.Dir(f.library), ".bindery-staging")
	moved := stage + "-moved"
	decoy := filepath.Join(t.TempDir(), "decoy")
	if err := os.Mkdir(decoy, 0o700); err != nil {
		t.Fatal(err)
	}
	called := false
	variantAfterAttemptHook = func(name string) {
		called = true
		if err := os.Rename(stage, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(decoy, stage); err != nil {
			t.Fatal(err)
		}
		trap := filepath.Join(decoy, name)
		if err := os.Mkdir(trap, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(trap, "keep"), []byte("not Bindery's stage"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { variantAfterAttemptHook = nil })
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	f.importSource(t, dl, source)
	if !called {
		t.Fatal("root rename hook was not exercised")
	}
	variantBlocked(t, f, dl, "staging root")
	variantImportedFiles(t, f, 0)
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("unverified stage published: %v", err)
	}
	entries, err := os.ReadDir(decoy)
	if err != nil || len(entries) != 1 {
		t.Fatalf("symlink target changed: %+v %v", entries, err)
	}
	trap := filepath.Join(decoy, entries[0].Name())
	if b, err := os.ReadFile(filepath.Join(trap, "keep")); err != nil || string(b) != "not Bindery's stage" {
		t.Fatalf("decoy was staged into or cleaned: %q %v", b, err)
	}
	entries, err = os.ReadDir(moved)
	if err != nil || len(entries) != 0 {
		t.Fatalf("verified stage cleanup failed: %+v %v", entries, err)
	}
}

func TestVariantStage_InterruptedPrivateAttemptSurvivesUnrelatedImport(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	stage := filepath.Join(filepath.Dir(f.library), ".bindery-staging")
	old := filepath.Join(stage, "interrupted-attempt")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(old, "keep")
	if err := os.WriteFile(marker, []byte("manual reconciliation"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	f.importSource(t, dl, source)
	variantImportedDownload(t, f, dl.ID)
	if b, err := os.ReadFile(marker); err != nil || string(b) != "manual reconciliation" {
		t.Fatalf("old private attempt touched: %q %v", b, err)
	}
	info, err := os.Lstat(stage)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("stage not private: %v %v", info, err)
	}
	entries, err := os.ReadDir(stage)
	if err != nil || len(entries) != 1 || entries[0].Name() != "interrupted-attempt" {
		t.Fatalf("staging cleanup touched other attempt or left new one: %+v %v", entries, err)
	}
}

func TestVariantStage_TrackFailurePreservesPublicLinkAndDoesNotNotifyABS(t *testing.T) {
	f := newVariantReviewFixture(t, "Dune", "Frank Herbert", models.MediaTypeAudiobook, "eng")
	folder, files := variantAudioSources(t, f)
	dest := variantAudioTarget(t, f)
	if _, err := f.database.ExecContext(f.ctx, `CREATE TRIGGER refuse_variant_file BEFORE INSERT ON book_files BEGIN SELECT RAISE(FAIL,'track variant failed'); END`); err != nil {
		t.Fatal(err)
	}
	notified := 0
	f.scanner.absLib = &variantABSCount{calls: &notified}
	f.scanner.absLibraryIDsFn = func() []string { return []string{"test"} }
	dl := f.addDownload(t, "Dune by Frank Herbert [ENG / M4B]")
	f.scanner.tryImportInternal(withTorrentOwnFolder(f.ctx, true), dl, folder, "qbittorrent", "hash", "", nil, files)
	variantBlocked(t, f, dl, "stranded verified public link")
	variantImportedFiles(t, f, 0)
	if notified != 0 {
		t.Fatalf("ABS notified before DB commit: %d", notified)
	}
	for _, p := range files {
		linked, err := os.Lstat(filepath.Join(dest, filepath.Base(p)))
		if err != nil || !sameFilePath(t, p, linked) {
			t.Fatalf("verified stranded link lost: %v", err)
		}
	}
}

type variantABSCount struct{ calls *int }

func (a *variantABSCount) ScanLibrary(_ context.Context, _ string) error { (*a.calls)++; return nil }

func TestVariantStage_TrackingTransactionRechecksPublicAndSourceBeforeCommit(t *testing.T) {
	for _, target := range []string{"public", "source"} {
		t.Run(target, func(t *testing.T) {
			f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
			source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
			dest := variantEbookTarget(t, f, source)
			variantCommitWindowHook = func() {
				if target == "source" {
					later := time.Now().Add(2 * time.Hour)
					if err := os.Chtimes(source, later, later); err != nil {
						t.Fatal(err)
					}
					return
				}
				if err := os.Remove(dest); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dest, []byte("other writer"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { variantCommitWindowHook = nil })
			dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
			f.importSource(t, dl, source)
			variantBlocked(t, f, dl, "stranded")
			variantImportedFiles(t, f, 0)
			if rows, err := f.books.VariantRepo().ListByBook(f.ctx, f.book.ID); err != nil || len(rows) != 1 || rows[0].Status == models.VariantImported {
				t.Fatalf("failed validation committed variant: %+v %v", rows, err)
			}
			if target == "public" {
				if b, err := os.ReadFile(dest); err != nil || string(b) != "other writer" {
					t.Fatalf("other writer's public link changed: %q %v", b, err)
				}
			}
		})
	}
}

func TestVariantStage_PublicReplacementBeforeTrackingIsNeverDeletedOrClaimed(t *testing.T) {
	f := newVariantReviewFixture(t, "English catalogue label", "Sample Author", models.MediaTypeEbook, "eng")
	source := variantEPUBSource(t, f.sources, "English catalogue label.epub", "english.epub")
	dest := variantEbookTarget(t, f, source)
	// The hook runs after the atomic promotion and before DB tracking.
	variantBeforeTrackHook = func() {
		if err := os.Remove(dest); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, []byte("other writer"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { variantBeforeTrackHook = nil })
	dl := f.addDownload(t, "English catalogue label [ENG / EPUB]")
	f.importSource(t, dl, source)
	variantBlocked(t, f, dl, "stranded")
	variantImportedFiles(t, f, 0)
	if b, err := os.ReadFile(dest); err != nil || string(b) != "other writer" {
		t.Fatalf("public replacement deleted: %q %v", b, err)
	}
}
