package importer

import (
	"net/http/httptest"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// createSibling adds a second download row naming the same torrent as the
// fixture's own row, the shape a torrent client produces when a second grab
// of the release adopts the torrent the first one added.
func (fx *removeOnImportFixture) createSibling(t *testing.T, client *models.DownloadClient, status models.DownloadState) *models.Download {
	t.Helper()
	id := removeOnImportHash
	dl := &models.Download{
		GUID:             "guid-2046-sibling-" + client.Type,
		Title:            fx.book.Title,
		NZBURL:           "magnet:?xt=urn:btih:" + removeOnImportHash,
		Status:           status,
		Protocol:         "torrent",
		TorrentID:        &id,
		DownloadClientID: &client.ID,
	}
	if err := fx.s.downloads.Create(fx.ctx, dl); err != nil {
		t.Fatalf("create sibling: %v", err)
	}
	return dl
}

// siblingCases for shared-torrent cleanup semantics. Transmission still drops
// the torrent once the last non-terminal sibling imports; qBittorrent is
// interlocked and keeps it regardless.
var siblingCases = []struct {
	name        string
	status      models.DownloadState
	wantRemoval bool
}{
	{"sibling importBlocked keeps the torrent", models.StateImportBlocked, false},
	{"sibling imported does not", models.StateImported, true},
}

func TestCheckQbittorrentDownloads_RemoveOnImportKeepsSharedTorrent(t *testing.T) {
	for _, sc := range qbitRemoveOnImportScenarios {
		for _, cats := range qbitCategoryMatrix {
			for _, sib := range siblingCases {
				t.Run(sc.name+"/configured="+cats.configured+"/torrent="+cats.torrent+"/"+sib.name, func(t *testing.T) {
					fx := newRemoveOnImportFixture(t)
					torrent, files, status := sc.setup(t, fx)
					torrent["category"] = cats.torrent
					fake := &fakeQbit{t: t, torrent: torrent, files: files}
					srv := httptest.NewServer(fake)
					defer srv.Close()

					client := fx.createClient(t, srv, "qbittorrent", true)
					client.Category = cats.configured
					dl := fx.createDownload(t, client, status, removeOnImportHash)
					other := fx.createSibling(t, client, sib.status)

					fx.s.checkQbittorrentDownloads(fx.ctx, client)

					if got := fx.status(t, dl.GUID); got.Status != qbitExpectedState(sc) {
						t.Fatalf("download status = %q (%s), want %q", got.Status, got.ErrorMessage, qbitExpectedState(sc))
					}
					if got := fx.status(t, other.GUID); got.Status != sib.status {
						t.Fatalf("sibling status changed to %q", got.Status)
					}
					reqs := fake.deleteRequests()
					if len(reqs) != 0 {
						t.Fatalf("qBittorrent interlock violated: torrent was removed: %v", reqs)
					}
				})
			}
		}
	}
}

func TestCheckTransmissionDownloads_RemoveOnImportKeepsSharedTorrent(t *testing.T) {
	for _, sib := range siblingCases {
		t.Run(sib.name, func(t *testing.T) {
			fx := newRemoveOnImportFixture(t)
			fake := &fakeTransmission{t: t, torrents: []map[string]any{fx.transmissionTorrent()}}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			client := fx.createClient(t, srv, "transmission", true)
			dl := fx.createDownload(t, client, models.StateDownloading, removeOnImportHash)
			other := fx.createSibling(t, client, sib.status)

			fx.s.checkTransmissionDownloads(fx.ctx, client)

			if got := fx.status(t, dl.GUID); got.Status != models.StateImported {
				t.Fatalf("download status = %q (%s), want imported", got.Status, got.ErrorMessage)
			}
			if got := fx.status(t, other.GUID); got.Status != sib.status {
				t.Fatalf("sibling status changed to %q", got.Status)
			}
			reqs := fake.removeRequests()
			if sib.wantRemoval && len(reqs) != 1 {
				t.Fatalf("torrent-remove calls = %d, want 1", len(reqs))
			}
			if !sib.wantRemoval && len(reqs) != 0 {
				t.Fatalf("the torrent was removed while another download still uses it: %v", reqs)
			}
		})
	}
}
