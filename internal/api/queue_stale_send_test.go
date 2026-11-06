package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/httpsec"
	"github.com/vavallee/bindery/internal/models"
	"github.com/vavallee/bindery/internal/notifier"
)

// A client error is still returned to the caller, but failure history and
// webhooks describe only a failure that actually changed the download row.
func TestQueueGrabSendFailureEventsRequireAcceptedTransition(t *testing.T) {
	for _, tc := range []struct {
		name          string
		concurrentEnd bool
		wantEvents    int
	}{
		{name: "stale terminal row", concurrentEnd: true, wantEvents: 0},
		{name: "accepted client failure", wantEvents: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(httpsec.AllowLoopbackForTests())
			ctx := context.Background()
			indexer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><nzb></nzb>`))
			}))
			t.Cleanup(indexer.Close)

			h, database, downloads, clients, _, _ := queueFixture(t)
			var sendCalls atomic.Int32
			sab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api" || r.URL.Query().Get("mode") != "addfile" {
					t.Errorf("unexpected client request: %s?%s", r.URL.Path, r.URL.RawQuery)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				sendCalls.Add(1)
				if tc.concurrentEnd {
					row, err := downloads.GetByGUID(ctx, "send-failure")
					if err != nil || row == nil {
						t.Errorf("reload row during send: %+v %v", row, err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					if err := downloads.SetError(ctx, row.ID, "newer failure reason"); err != nil {
						t.Errorf("record newer terminal result: %v", err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": false})
			}))
			t.Cleanup(sab.Close)
			host, port := testServerHostPort(t, sab.URL)
			if err := clients.Create(ctx, &models.DownloadClient{Name: "sab", Type: "sabnzbd", Host: host, Port: port, Enabled: true}); err != nil {
				t.Fatal(err)
			}

			var webhookCalls atomic.Int32
			webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["eventType"] != notifier.EventDownloadFailed {
					t.Errorf("unexpected webhook: %+v %v", payload, err)
				}
				webhookCalls.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(webhook.Close)
			notifications := db.NewNotificationRepo(database)
			if err := notifications.Create(ctx, &models.Notification{
				Name: "failure hook", Type: "webhook", URL: webhook.URL, Method: http.MethodPost, OnFailure: true, Enabled: true,
			}); err != nil {
				t.Fatal(err)
			}
			n := notifier.New(notifications)
			n.SetValidator(nil)
			h.WithNotifier(n)

			_, err := h.grab(ctx, grabRequest{GUID: "send-failure", Title: "Failed Book.epub", NZBURL: indexer.URL + "/file.nzb"})
			if !errors.Is(err, errSendFailed) {
				t.Fatalf("send result = %v, want send failure", err)
			}
			if sendCalls.Load() != 1 {
				t.Fatalf("client received %d sends, want one", sendCalls.Load())
			}
			row, err := downloads.GetByGUID(ctx, "send-failure")
			if err != nil || row == nil || row.Status != models.StateFailed {
				t.Fatalf("download after failed send: %+v %v", row, err)
			}
			if tc.concurrentEnd && row.ErrorMessage != "newer failure reason" {
				t.Errorf("stale send overwrote terminal reason: %q", row.ErrorMessage)
			}
			if !tc.concurrentEnd && !strings.Contains(row.ErrorMessage, "SABnzbd rejected download") {
				t.Errorf("accepted failure lost client reason: %q", row.ErrorMessage)
			}
			events, err := db.NewHistoryRepo(database).ListByType(ctx, models.HistoryEventDownloadFailed)
			if err != nil || len(events) != tc.wantEvents {
				t.Errorf("downloadFailed history = %+v, %v; want %d", events, err, tc.wantEvents)
			}
			if got := int(webhookCalls.Load()); got != tc.wantEvents {
				t.Errorf("downloadFailed webhooks = %d, want %d", got, tc.wantEvents)
			}
		})
	}
}
