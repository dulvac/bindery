package downloader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

func TestRemoveDownload_QbitTorrentProtectedBeforeClientCall(t *testing.T) {
	for _, category := range []string{"mam", "books"} {
		for _, deleteFiles := range []bool{false, true} {
			t.Run(category+"/deleteFiles="+map[bool]string{true: "true", false: "false"}[deleteFiles], func(t *testing.T) {
				var calls []string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls = append(calls, r.URL.Path)
					if r.URL.Path == "/api/v2/auth/login" {
						_, _ = w.Write([]byte("Ok."))
					}
				}))
				defer srv.Close()
				host, port := serverHostPort(t, srv.URL)
				client := &models.DownloadClient{Type: "qbittorrent", Host: host, Port: port, Category: category, Username: "fake", Password: "fake"}
				hash := "2046204620462046204620462046204620462046"
				dl := &models.Download{TorrentID: &hash, Protocol: "torrent"}
				err := RemoveDownload(context.Background(), client, dl, deleteFiles, "")
				if err == nil || !strings.Contains(err.Error(), "protected qBittorrent torrent") {
					t.Fatalf("expected protected-torrent refusal, got %v", err)
				}
				if len(calls) != 0 {
					t.Fatalf("client contacted before protection check: %v", calls)
				}
			})
		}
	}
}
