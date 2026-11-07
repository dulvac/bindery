package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/models"
)

// A Pending grab spends ratio only after an explicit acknowledgement. Both
// legacy and variant releases must keep their row and never reach the fake
// downloader on any missing or malformed acknowledgement.
func TestPendingGrabRequiresRatioAcknowledgementBeforeClientSend(t *testing.T) {
	for _, hasVariants := range []bool{false, true} {
		name := "legacy"
		if hasVariants {
			name = "variant"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			fixture := func(t *testing.T) (*covQPendingGrab, string, int64) {
				t.Helper()
				f := covQNewPendingGrab(t, models.MediaTypeEbook)
				if !hasVariants {
					return f, "", 0
				}
				f.book.MediaType = models.MediaTypeEbook
				if err := f.h.books.Update(ctx, f.book); err != nil {
					t.Fatal(err)
				}
				copies, err := f.h.books.VariantRepo().EnsureWanted(ctx, f.book.ID, models.MediaTypeEbook, []string{"eng"})
				if err != nil {
					t.Fatal(err)
				}
				return f, `"variantId":` + strconv.FormatInt(copies[0].ID, 10) + `,`, copies[0].ID
			}
			grab := func(t *testing.T, f *covQPendingGrab, body string, noBody bool) *httptest.ResponseRecorder {
				t.Helper()
				id := strconv.FormatInt(f.prID, 10)
				var r *http.Request
				if noBody {
					r = httptest.NewRequest(http.MethodPost, "/api/v1/pending/"+id+"/grab", nil)
				} else {
					r = httptest.NewRequest(http.MethodPost, "/api/v1/pending/"+id+"/grab", strings.NewReader(body))
				}
				return f.grab(t, withURLParam(r, "id", id))
			}
			for _, tc := range []struct {
				name   string
				fields string
				noBody bool
				raw    bool
			}{
				{"absent body", "", true, false},
				{"empty body", "", false, true},
				{"missing acknowledgement", `"other":"ignored"`, false, false},
				{"false", `"acknowledgeRatioCost":false`, false, false},
				{"null", `"acknowledgeRatioCost":null`, false, false},
				{"string", `"acknowledgeRatioCost":"true"`, false, false},
				{"number", `"acknowledgeRatioCost":1`, false, false},
				{"null request", `null`, false, true},
				{"trailing object", `"acknowledgeRatioCost":true}{`, false, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					f, variantField, _ := fixture(t)
					body := tc.fields
					if !tc.raw {
						body = `{` + variantField + body + `}`
					}
					rec := grab(t, f, body, tc.noBody)
					if rec.Code != http.StatusBadRequest {
						t.Errorf("status %d: %s; want 400", rec.Code, rec.Body.String())
					}
					if f.adds.calls != 0 {
						t.Errorf("fake downloader called %d times on refusal", f.adds.calls)
					}
					if left, err := f.pending.List(ctx); err != nil || len(left) != 1 || left[0].ID != f.prID {
						t.Errorf("pending row lost on refusal: %+v, %v", left, err)
					}
					if downloads, err := f.downloads.List(ctx); err != nil || len(downloads) != 0 {
						t.Errorf("download row created on refusal: %+v, %v", downloads, err)
					}
				})
			}
			f, variantField, variantID := fixture(t)
			if hasVariants {
				if rec := grab(t, f, `{"variantId":0,"acknowledgeRatioCost":true}`, false); rec.Code != http.StatusBadRequest {
					t.Fatalf("zero variant = %d: %s; want 400", rec.Code, rec.Body.String())
				}
				if rec := grab(t, f, `{"acknowledgeRatioCost":true}`, false); rec.Code != http.StatusBadRequest {
					t.Fatalf("missing variant = %d: %s; want 400", rec.Code, rec.Body.String())
				}
				if f.adds.calls != 0 {
					t.Fatalf("fake downloader called %d times without a real variant", f.adds.calls)
				}
			}
			rec := grab(t, f, `{`+variantField+`"acknowledgeRatioCost":true}`, false)
			if rec.Code != http.StatusCreated {
				t.Fatalf("acknowledged grab = %d: %s; want 201", rec.Code, rec.Body.String())
			}
			if f.adds.calls != 1 {
				t.Fatalf("acknowledged grab sent %d times; want once", f.adds.calls)
			}
			if left, err := f.pending.List(ctx); err != nil || len(left) != 0 {
				t.Fatalf("acknowledged grab left pending row: %+v, %v", left, err)
			}
			if downloads, err := f.downloads.List(ctx); err != nil || len(downloads) != 1 || (hasVariants && (downloads[0].VariantID == nil || *downloads[0].VariantID != variantID)) {
				t.Fatalf("acknowledged grab download association: %+v, %v", downloads, err)
			}
		})
	}
}

// Invalid acknowledgement takes priority over stored-release or downloader
// availability errors for an existing, owned Pending row.
func TestPendingGrabRejectsMissingAcknowledgementBeforeStoredReleaseAndQueue(t *testing.T) {
	for _, broken := range []string{"missing queue", "corrupt release"} {
		t.Run(broken, func(t *testing.T) {
			f := covQNewPendingGrab(t, models.MediaTypeEbook)
			h := f.h
			if broken == "missing queue" {
				h = NewPendingHandler(f.pending, nil, f.downloads, f.h.books)
			} else if _, err := f.database.Exec("UPDATE pending_releases SET release_json = '{not json' WHERE id = ?", f.prID); err != nil {
				t.Fatal(err)
			}
			id := strconv.FormatInt(f.prID, 10)
			rec := httptest.NewRecorder()
			h.Grab(rec, withURLParam(httptest.NewRequest(http.MethodPost, "/api/v1/pending/"+id+"/grab", nil), "id", id))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: status %d: %s; want 400", broken, rec.Code, rec.Body.String())
			}
			if f.adds.calls != 0 {
				t.Fatalf("%s: fake downloader called %d times", broken, f.adds.calls)
			}
			if left, err := f.pending.List(context.Background()); err != nil || len(left) != 1 {
				t.Fatalf("%s: pending release lost: %+v, %v", broken, left, err)
			}
		})
	}
}
