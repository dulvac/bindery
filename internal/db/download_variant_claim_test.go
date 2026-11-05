package db

import (
	"context"
	"sync"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// Two independent searches may choose different GUIDs for the same copy.
// Only one can reserve an active download, even before either client send.
func TestVariantDownloadClaimOnlyOneActive(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	variants := NewBookVariantRepo(database)
	copies, err := variants.EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for _, guid := range []string{"claim-a", "claim-b"} {
		wg.Add(1)
		go func(guid string) {
			defer wg.Done()
			d := &models.Download{GUID: guid, BookID: &book.ID, VariantID: &copies[0].ID,
				Title: "Sample Book [ENG / EPUB]", Protocol: "usenet", Status: models.StateGrabbed}
			if err := NewDownloadRepo(database).Create(ctx, d); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(guid)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("same variant reserved by %d simultaneous downloads, want one", wins)
	}
	rows, err := NewDownloadRepo(database).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].VariantID == nil || *rows[0].VariantID != copies[0].ID {
		t.Fatalf("reserved downloads = %+v", rows)
	}
	// The sending operation can fail after reserving the row. A failed row
	// releases the claim, leaving the variant wanted for another release.
	if err := NewDownloadRepo(database).SetError(ctx, rows[0].ID, "fake downloader unavailable"); err != nil {
		t.Fatal(err)
	}
	other := &models.Download{GUID: "claim-c", BookID: &book.ID, VariantID: &copies[0].ID,
		Title: "Another edition [ENG / EPUB]", Protocol: "usenet", Status: models.StateGrabbed}
	if err := NewDownloadRepo(database).Create(ctx, other); err != nil {
		t.Fatalf("failed send held the variant: %v", err)
	}
}

func TestVariantDownloadClaimGuardsDirectActiveCreate(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	copies, err := NewBookVariantRepo(database).EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	first := &models.Download{GUID: "first-grab", BookID: &book.ID, VariantID: &copies[0].ID,
		Title: "Sample Book [ENG / EPUB]", Protocol: "usenet", Status: models.StateGrabbed}
	if err := NewDownloadRepo(database).Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := &models.Download{GUID: "direct-downloading", BookID: &book.ID, VariantID: &copies[0].ID,
		Title: "Sample Book [ENG / EPUB]", Protocol: "usenet", Status: models.StateDownloading}
	if err := NewDownloadRepo(database).Create(ctx, second); err == nil {
		t.Fatal("direct active creation bypassed the variant claim")
	}
}

func TestVariantDownloadClaimRejectsAlreadyImportedVariant(t *testing.T) {
	database, _, book := openTestDB(t)
	ctx := context.Background()
	variants := NewBookVariantRepo(database)
	copies, err := variants.EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	if err := variants.SetStatus(ctx, copies[0].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	d := &models.Download{GUID: "claim-imported", BookID: &book.ID, VariantID: &copies[0].ID,
		Title: "Sample Book [ENG / EPUB]", Protocol: "usenet", Status: models.StateGrabbed}
	if err := NewDownloadRepo(database).Create(ctx, d); err == nil {
		t.Fatal("imported variant gained another active download")
	}
}
