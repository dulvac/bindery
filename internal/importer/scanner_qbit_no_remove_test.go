package importer

import (
	"context"
	"testing"

	"github.com/vavallee/bindery/internal/downloader/qbittorrent"
	"github.com/vavallee/bindery/internal/models"
)

// TestQbittorrentRemoveOnImportCleanup_AlwaysNil locks Task 7A's hard stop:
// qBittorrent remove-on-import cleanup is disabled regardless of mutable
// metadata (client type/category/toggle drift).
func TestQbittorrentRemoveOnImportCleanup_AlwaysNil(t *testing.T) {
	fx := newRemoveOnImportFixture(t)
	qb := qbittorrent.New("127.0.0.1", 8080, "", "", "", false)
	hash := removeOnImportHash
	dl := &models.Download{Title: "Title T", TorrentID: &hash}

	cases := []struct {
		name   string
		client *models.DownloadClient
	}{
		{name: "qbittorrent type", client: &models.DownloadClient{Name: "qb", Type: "qbittorrent", RemoveOnImport: true, Category: "mam"}},
		{name: "transmission type", client: &models.DownloadClient{Name: "tx", Type: "transmission", RemoveOnImport: true, Category: "books"}},
		{name: "empty type", client: &models.DownloadClient{Name: "empty", Type: "", RemoveOnImport: true, Category: "books"}},
		{name: "toggle off", client: &models.DownloadClient{Name: "qb-off", Type: "qbittorrent", RemoveOnImport: false, Category: "mam"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleanup := fx.s.qbittorrentRemoveOnImportCleanup(context.Background(), qb, tc.client, dl)
			if cleanup != nil {
				t.Fatalf("cleanup callback must be nil for %s", tc.name)
			}
		})
	}
}
