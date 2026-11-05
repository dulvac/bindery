package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

func assertRequestLanguages(t *testing.T, f *requestsFixture, rec *httptest.ResponseRecorder, want []string) int64 {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", rec.Code, rec.Body.String())
	}
	var response struct {
		ID        int64    `json:"id"`
		Languages []string `json:"languages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(response.Languages, want) {
		t.Fatalf("response languages = %v, want %v", response.Languages, want)
	}
	var stored string
	if err := f.database.QueryRow(`SELECT languages_json FROM requests WHERE id = ?`, response.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var rowLanguages []string
	if err := json.Unmarshal([]byte(stored), &rowLanguages); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rowLanguages, want) {
		t.Fatalf("stored languages = %v, want %v", rowLanguages, want)
	}
	row, err := f.requests.GetByID(context.Background(), response.ID)
	if err != nil || row == nil {
		t.Fatalf("read request: %+v %v", row, err)
	}
	var payload struct {
		Languages []string `json:"languages"`
	}
	if err := json.Unmarshal([]byte(row.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(payload.Languages, want) {
		t.Fatalf("approval payload languages = %v, row = %v", payload.Languages, want)
	}
	return response.ID
}

func requestBookBody(languages string) string {
	return `{"kind":"book","foreignId":"OL27482W","mediaType":"ebook"` + languages + `}`
}

func TestRequestLanguagesValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       []string
	}{
		{"omitted defaults to English", requestBookBody(""), http.StatusCreated, []string{"eng"}},
		{"explicit null", requestBookBody(`,"languages":null`), http.StatusBadRequest, nil},
		{"null with whitespace", requestBookBody(`,"languages":  null  `), http.StatusBadRequest, nil},
		{"empty", requestBookBody(`,"languages":[]`), http.StatusBadRequest, nil},
		{"unknown code", requestBookBody(`,"languages":["de"]`), http.StatusBadRequest, nil},
		{"wrong JSON type", requestBookBody(`,"languages":"en"`), http.StatusBadRequest, nil},
		{"aliases and duplicates", requestBookBody(`,"languages":["ro","en","ro"]`), http.StatusCreated, []string{"eng", "rum"}},
		{"author with languages", `{"kind":"author","foreignId":"OL39307A","languages":["en"]}`, http.StatusBadRequest, nil},
		{"author with null", `{"kind":"author","foreignId":"OL39307A","languages":  null}`, http.StatusBadRequest, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := wellsRequestStub()
			lookups := make(chan struct{}, 1)
			provider.getBookEntered = lookups
			f := newRequestsFixture(t, provider, nil)
			rec := f.create(t, f.requester, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("create = %d %s, want %d", rec.Code, rec.Body.String(), tc.status)
			}
			if tc.status == http.StatusCreated {
				assertRequestLanguages(t, f, rec, tc.want)
			} else {
				if n, err := f.requests.CountPending(context.Background()); err != nil || n != 0 {
					t.Fatalf("invalid request stored %d rows: %v", n, err)
				}
				select {
				case <-lookups:
					t.Fatal("invalid language reached metadata provider")
				default:
				}
			}
		})
	}
}

func TestRequestLanguagesPendingMergeAndForUserAtCap(t *testing.T) {
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	if err := f.settings.Set(context.Background(), SettingRequestsMaxPendingPerUser, "1"); err != nil {
		t.Fatal(err)
	}
	first := assertRequestLanguages(t, f, f.create(t, f.requester, requestBookBody(`,"languages":["en"]`)), []string{"eng"})
	body := `{"kind":"book","foreignId":"OL27482W","mediaType":"ebook","languages":["ro"],"forUser":` + strconv.FormatInt(f.requester.ID, 10) + `}`
	second := assertRequestLanguages(t, f, f.createWithKey(t, body), []string{"eng", "rum"})
	if first != second {
		t.Fatalf("merged into row %d instead of pending row %d", second, first)
	}
	if n, err := f.requests.CountPendingByOwner(context.Background(), f.requester.ID); err != nil || n != 1 {
		t.Fatalf("pending count = %d, want one: %v", n, err)
	}
	if q := f.queue(t); len(q) != 1 || q[0].ID != first {
		t.Fatalf("queue = %+v, want one merged request", q)
	}
	row, err := f.requests.GetByID(context.Background(), first)
	if err != nil || row.OwnerUserID != f.requester.ID || row.CreatedVia != "" {
		t.Fatalf("merge changed attribution/owner: %+v %v", row, err)
	}
	if rec := f.create(t, f.requester, requestBookBody(`,"languages":["en"]`)); rec.Code != http.StatusConflict {
		t.Fatalf("identical pending choice = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if rec := f.create(t, f.requester, requestBookBody(`,"languages":["fr"]`)); rec.Code != http.StatusCreated {
		t.Fatalf("merge despite cap = %d %s", rec.Code, rec.Body.String())
	} else {
		assertRequestLanguages(t, f, rec, []string{"eng", "fre", "rum"})
	}
}

// An omitted format still requests the provider's format (or the configured
// default). A later explicit format must not narrow that first selection.
func TestRequestLanguagesPendingMergePreservesOmittedFormat(t *testing.T) {
	for _, tc := range []struct {
		name, providerFormat string
		defaultFormat        string
		legacyEmpty          bool
	}{
		{name: "provider supports both", providerFormat: models.MediaTypeBoth},
		{name: "provider has no format, default is both", defaultFormat: models.MediaTypeBoth},
		{name: "older pending row with omitted format", providerFormat: models.MediaTypeBoth, legacyEmpty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := wellsRequestStub()
			provider.getBookByID["OL27482W"].MediaType = tc.providerFormat
			f := newRequestsFixture(t, provider, nil)
			if tc.defaultFormat != "" {
				if err := f.settings.Set(context.Background(), SettingDefaultMediaType, tc.defaultFormat); err != nil {
					t.Fatal(err)
				}
			}
			first := assertRequestLanguages(t, f, f.create(t, f.requester, `{"kind":"book","foreignId":"OL27482W","languages":["en"]}`), []string{"eng"})
			if tc.legacyEmpty {
				// Pending rows written before this fix retained an empty format.
				if _, err := f.database.Exec(`UPDATE requests SET media_type = '', payload_json = json_set(payload_json, '$.mediaType', '') WHERE id = ?`, first); err != nil {
					t.Fatal(err)
				}
			}
			second := assertRequestLanguages(t, f, f.create(t, f.requester, requestBookBody(`,"languages":["ro"]`)), []string{"eng", "rum"})
			if second != first {
				t.Fatalf("merge created request %d instead of reusing %d", second, first)
			}
			row, err := f.requests.GetByID(context.Background(), first)
			if err != nil || row == nil {
				t.Fatalf("merged request = %+v %v", row, err)
			}
			var payload requestPayload
			if err := json.Unmarshal([]byte(row.PayloadJSON), &payload); err != nil {
				t.Fatal(err)
			}
			if row.MediaType != models.MediaTypeBoth || payload.MediaType != models.MediaTypeBoth {
				t.Fatalf("merge narrowed omitted format: row %q, payload %q; want both", row.MediaType, payload.MediaType)
			}
			approved := f.approve(f.admin, first, `{}`)
			if approved.Code != http.StatusOK {
				t.Fatalf("approval = %d %s", approved.Code, approved.Body.String())
			}
			bookID := *decodeRequest(t, approved).ResultBookID
			variants, err := db.NewBookVariantRepo(f.database).ListByBook(context.Background(), bookID)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(variants))
			for _, v := range variants {
				got = append(got, v.MediaType+"/"+v.Language)
			}
			sort.Strings(got)
			want := []string{"audiobook/eng", "audiobook/rum", "ebook/eng", "ebook/rum"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("approved variants = %v, want %v", got, want)
			}
		})
	}
}

// Resolving an older empty-format row must not turn a repeat selection into
// a new request merely because its persisted format becomes explicit.
func TestRequestLanguagesLegacyOmittedFormatRepeatConflicts(t *testing.T) {
	provider := wellsRequestStub()
	provider.getBookByID["OL27482W"].MediaType = models.MediaTypeBoth
	f := newRequestsFixture(t, provider, nil)
	first := assertRequestLanguages(t, f, f.create(t, f.requester, `{"kind":"book","foreignId":"OL27482W","languages":["en"]}`), []string{"eng"})
	if _, err := f.database.Exec(`UPDATE requests SET media_type = '', payload_json = json_set(payload_json, '$.mediaType', '') WHERE id = ?`, first); err != nil {
		t.Fatal(err)
	}
	if rec := f.create(t, f.requester, requestBookBody(`,"languages":["en"]`)); rec.Code != http.StatusConflict {
		t.Fatalf("repeat ebook already included in omitted both = %d %s, want 409", rec.Code, rec.Body.String())
	}
}

// Both submissions see no row, but the second lookup finishes after the first
// insert has filled the pending cap. The same book may still merge; another
// book must remain capped. The gate fixes the interleaving without sleeps.
func TestRequestLanguagesConcurrentInitialCreatesMergeAtCap(t *testing.T) {
	for _, tc := range []struct {
		name    string
		forUser bool
	}{
		{name: "requester"},
		{name: "admin API key for requester", forUser: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRequestsFixture(t, wellsRequestStub(), nil)
			if err := f.settings.Set(context.Background(), SettingRequestsMaxPendingPerUser, "1"); err != nil {
				t.Fatal(err)
			}
			provider := wellsRequestStub()
			entered, gate := make(chan struct{}, 1), make(chan struct{})
			provider.getBookEntered, provider.getBookGate = entered, gate
			var release sync.Once
			defer release.Do(func() { close(gate) })
			late := NewRequestHandler(f.requests, f.books, f.authors, f.settings, f.users, provider, f.author).
				WithProviderLimiter(auth.NewRequesterLimiter(1<<20, 1<<20, time.Minute, 64))
			body := `{"kind":"book","foreignId":"OL27482W","mediaType":"ebook","languages":["ro"]`
			if tc.forUser {
				body += `,"forUser":` + strconv.FormatInt(f.requester.ID, 10)
			}
			body += `}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/requests", strings.NewReader(body))
			if tc.forUser {
				req = req.WithContext(auth.WithAPIKeyAuth(auth.WithUserRole(auth.WithUserID(req.Context(), f.admin.ID), auth.RoleAdmin)))
			} else {
				req = asUser(req, f.requester)
			}
			lateResult := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				rec := httptest.NewRecorder()
				late.Create(rec, req)
				lateResult <- rec
			}()
			select {
			case <-entered:
				// The second submission has passed the no-row and cap checks.
			case rec := <-lateResult:
				t.Fatalf("second submission returned before metadata gate: %d %s", rec.Code, rec.Body.String())
			case <-time.After(5 * time.Second):
				t.Fatal("second submission did not enter metadata lookup")
			}
			first := assertRequestLanguages(t, f, f.create(t, f.requester, requestBookBody(`,"languages":["en"]`)), []string{"eng"})
			release.Do(func() { close(gate) })
			var rec *httptest.ResponseRecorder
			select {
			case rec = <-lateResult:
			case <-time.After(5 * time.Second):
				t.Fatal("second submission did not finish")
			}
			second := assertRequestLanguages(t, f, rec, []string{"eng", "rum"})
			if second != first {
				t.Fatalf("second submission created row %d instead of merging into %d", second, first)
			}
			row, err := f.requests.GetByID(context.Background(), first)
			if err != nil || row == nil || row.OwnerUserID != f.requester.ID || row.CreatedVia != "" {
				t.Fatalf("merge changed owner or first asker's attribution: %+v %v", row, err)
			}
			if n, err := f.requests.CountPendingByOwner(context.Background(), f.requester.ID); err != nil || n != 1 {
				t.Fatalf("pending count = %d, want one: %v", n, err)
			}
			if blocked := f.create(t, f.requester, `{"kind":"book","foreignId":"OL-OTHER"}`); blocked.Code != http.StatusTooManyRequests {
				t.Fatalf("different book at cap = %d %s, want 429", blocked.Code, blocked.Body.String())
			}
		})
	}
}

func TestRequestLanguagesApprovedOwnedBookExtendsAndFulfilsEveryVariant(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	ctx := context.Background()
	first := assertRequestLanguages(t, f, f.create(t, f.requester, requestBookBody(`,"languages":["en"]`)), []string{"eng"})
	approved := f.approve(f.admin, first, `{}`)
	if approved.Code != http.StatusOK {
		t.Fatalf("first approval = %d %s", approved.Code, approved.Body.String())
	}
	firstResult := decodeRequest(t, approved)
	if firstResult.ResultBookID == nil {
		t.Fatal("approval did not create book")
	}
	book, err := f.books.GetByID(ctx, *firstResult.ResultBookID)
	if err != nil || book == nil || book.OwnerUserID != f.requester.ID {
		t.Fatalf("book owner = %+v %v", book, err)
	}
	variants := db.NewBookVariantRepo(f.database)
	v, err := variants.ListByBook(ctx, book.ID)
	if err != nil || len(v) != 1 || v[0].Language != "eng" {
		t.Fatalf("initial variants = %+v %v", v, err)
	}
	if err := variants.SetStatus(ctx, v[0].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	book.Status = models.BookStatusImported
	if err := f.books.Update(ctx, book); err != nil {
		t.Fatal(err)
	}
	second := assertRequestLanguages(t, f, f.create(t, f.requester, requestBookBody(`,"languages":["ro"]`)), []string{"rum"})
	if second != first {
		t.Fatalf("reopen id = %d, want %d", second, first)
	}
	row, err := f.requests.GetByID(ctx, first)
	if err != nil || row.Status != models.RequestStatusPending || row.ResultBookID != nil {
		t.Fatalf("reopened row = %+v %v", row, err)
	}
	approved = f.approve(f.admin, first, `{}`)
	if approved.Code != http.StatusOK {
		t.Fatalf("extend approval = %d %s", approved.Code, approved.Body.String())
	}
	if got := decodeRequest(t, approved).ResultBookID; got == nil || *got != book.ID {
		t.Fatalf("extension book id = %v, want %d", got, book.ID)
	}
	v, err = variants.ListByBook(ctx, book.ID)
	if err != nil || len(v) != 2 || v[0].Language != "eng" || v[1].Language != "rum" {
		t.Fatalf("extended variants = %+v %v", v, err)
	}
	if n := countBookDownloads(t, f, book.ID); n != 0 {
		t.Fatalf("extension created %d torrents", n)
	}
	if mine := f.listMine(t, f.requester); len(mine) != 1 || mine[0].Fulfilled {
		t.Fatalf("Romanian variant still wanted, request must be unfulfilled: %+v", mine)
	}
	if err := variants.SetStatus(ctx, v[1].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	if mine := f.listMine(t, f.requester); len(mine) != 1 || !mine[0].Fulfilled {
		t.Fatalf("both variants imported, request must be fulfilled: %+v", mine)
	}
}

func countBookDownloads(t *testing.T, f *requestsFixture, bookID int64) int {
	t.Helper()
	var n int
	if err := f.database.QueryRow(`SELECT COUNT(*) FROM downloads WHERE book_id = ?`, bookID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRequestLanguagesEveryFormatAndLanguageMustBeImported(t *testing.T) {
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	body := `{"kind":"book","foreignId":"OL27482W","mediaType":"both","languages":["en","ro"]}`
	created := assertRequestLanguages(t, f, f.create(t, f.requester, body), []string{"eng", "rum"})
	approved := f.approve(f.admin, created, `{}`)
	if approved.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", approved.Code, approved.Body.String())
	}
	bookID := *decodeRequest(t, approved).ResultBookID
	variants := db.NewBookVariantRepo(f.database)
	rows, err := variants.ListByBook(context.Background(), bookID)
	if err != nil || len(rows) != 4 {
		t.Fatalf("both formats and languages = %+v %v, want four", rows, err)
	}
	for _, v := range rows[:3] {
		if err := variants.SetStatus(context.Background(), v.ID, models.VariantImported); err != nil {
			t.Fatal(err)
		}
	}
	book, err := f.books.GetByID(context.Background(), bookID)
	if err != nil {
		t.Fatal(err)
	}
	book.Status = models.BookStatusImported
	if err := f.books.Update(context.Background(), book); err != nil {
		t.Fatal(err)
	}
	if mine := f.listMine(t, f.requester); len(mine) != 1 || mine[0].Fulfilled {
		t.Fatalf("one variant wanted but book imported: %+v", mine)
	}
	if q := f.queue(t); len(q) != 1 || q[0].Fulfilled {
		t.Fatalf("queue reported a partial variant set fulfilled: %+v", q)
	}
	if err := variants.SetStatus(context.Background(), rows[3].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	if mine := f.listMine(t, f.requester); len(mine) != 1 || !mine[0].Fulfilled {
		t.Fatalf("all variants imported but request not fulfilled: %+v", mine)
	}
}

func TestRequestLanguagesApprovalRejectsPayloadMismatch(t *testing.T) {
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	created := assertRequestLanguages(t, f, f.create(t, f.requester, requestBookBody(`,"languages":["en","ro"]`)), []string{"eng", "rum"})
	if _, err := f.database.Exec(`UPDATE requests SET payload_json = json_set(payload_json, '$.languages', json('["fre"]')) WHERE id = ?`, created); err != nil {
		t.Fatal(err)
	}
	if rec := f.approve(f.admin, created, `{}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched approval payload = %d %s, want 422", rec.Code, rec.Body.String())
	}
	if got, err := f.books.GetByForeignID(context.Background(), "OL27482W"); err != nil || got != nil {
		t.Fatalf("tampered request created book %+v: %v", got, err)
	}
}

func TestRequestLanguagesConcurrentPendingMerges(t *testing.T) {
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	first := assertRequestLanguages(t, f, f.create(t, f.requester, requestBookBody(`,"languages":["en"]`)), []string{"eng"})
	var wg sync.WaitGroup
	start := make(chan struct{})
	codes := make([]int, 2)
	for i, lang := range []string{"ro", "fr"} {
		wg.Add(1)
		go func(i int, lang string) {
			defer wg.Done()
			<-start
			codes[i] = f.create(t, f.requester, requestBookBody(`,"languages":["`+lang+`"]`)).Code
		}(i, lang)
	}
	close(start)
	wg.Wait()
	if !reflect.DeepEqual(codes, []int{http.StatusCreated, http.StatusCreated}) {
		t.Fatalf("concurrent merges = %v, want both 201", codes)
	}
	row, err := f.requests.GetByID(context.Background(), first)
	if err != nil || row == nil {
		t.Fatalf("merged row = %+v %v", row, err)
	}
	var stored string
	if err := f.database.QueryRow(`SELECT languages_json FROM requests WHERE id = ?`, first).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var langs []string
	if err := json.Unmarshal([]byte(stored), &langs); err != nil || !reflect.DeepEqual(langs, []string{"eng", "fre", "rum"}) {
		t.Fatalf("merged languages = %v, want all three: %v", langs, err)
	}
	var payload struct {
		Languages []string `json:"languages"`
	}
	if err := json.Unmarshal([]byte(row.PayloadJSON), &payload); err != nil || !reflect.DeepEqual(payload.Languages, langs) {
		t.Fatalf("payload after race = %v, row = %v: %v", payload.Languages, langs, err)
	}
}

// An admin may override the format at approval. Check the effective format,
// not the requester's original one, when deciding whether a variant is missing.
func TestRequestLanguagesPendingMergeHandlesJSONWhitespace(t *testing.T) {
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	first := assertRequestLanguages(t, f, f.create(t, f.requester, requestBookBody("")), []string{"eng"})
	if _, err := f.database.Exec(`UPDATE requests SET languages_json = '[ "eng" ]' WHERE id = ?`, first); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	merged, err := f.requests.MergePending(ctx, first, f.requester.ID, "ebook", []string{"rum"}, "ebook")
	if err != nil || merged == nil || !reflect.DeepEqual(merged.Languages, []string{"eng", "rum"}) {
		t.Fatalf("merge valid noncanonical JSON = %+v %v, want eng+rum", merged, err)
	}
}

func TestRequestLanguagesApprovalMediaOverrideExtendsMissingFormat(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	created := assertRequestLanguages(t, f, f.create(t, f.requester, requestBookBody(`,"languages":["ro"]`)), []string{"rum"})
	body := `{"foreignBookId":"OL27482W","foreignAuthorId":"OL39307A","mediaType":"ebook","languages":["ro"]}`
	rec := httptest.NewRecorder()
	f.author.AddBook(rec, asUser(httptest.NewRequest(http.MethodPost, "/api/v1/author/book", strings.NewReader(body)), f.requester))
	if rec.Code != http.StatusCreated {
		t.Fatalf("direct ebook add = %d %s", rec.Code, rec.Body.String())
	}
	approved := f.approve(f.admin, created, `{"mediaType":"audiobook"}`)
	if approved.Code != http.StatusOK {
		t.Fatalf("approve missing audio variant = %d %s", approved.Code, approved.Body.String())
	}
	bookID := *decodeRequest(t, approved).ResultBookID
	variants, err := db.NewBookVariantRepo(f.database).ListByBook(context.Background(), bookID)
	if err != nil || len(variants) != 2 || variants[0].MediaType != "audiobook" || variants[1].MediaType != "ebook" {
		t.Fatalf("new audio variant on existing book = %+v %v", variants, err)
	}
	if err := db.NewBookVariantRepo(f.database).SetStatus(context.Background(), variants[1].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	if mine := f.listMine(t, f.requester); len(mine) != 1 || mine[0].Fulfilled {
		t.Fatalf("approval changed format to audio but unimported audio is fulfilled: %+v", mine)
	}
	if err := db.NewBookVariantRepo(f.database).SetStatus(context.Background(), variants[0].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	if mine := f.listMine(t, f.requester); len(mine) != 1 || !mine[0].Fulfilled {
		t.Fatalf("requested audio was imported but request not fulfilled: %+v", mine)
	}
}

func TestAddBookLanguagesConcurrentDirectAndRequesterApproval(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	first := f.create(t, f.requester, requestBookBody(`,"languages":["en"]`))
	id := assertRequestLanguages(t, f, first, []string{"eng"})
	if rec := f.approve(f.admin, id, `{}`); rec.Code != http.StatusOK {
		t.Fatalf("initial approval = %d %s", rec.Code, rec.Body.String())
	}
	pending := assertRequestLanguages(t, f, f.create(t, f.requester, requestBookBody(`,"languages":["ro"]`)), []string{"rum"})
	codes := make([]int, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		body := `{"foreignBookId":"OL27482W","foreignAuthorId":"OL39307A","languages":["ro"],"mediaType":"ebook"}`
		rec := httptest.NewRecorder()
		f.author.AddBook(rec, asUser(httptest.NewRequest(http.MethodPost, "/api/v1/author/book", strings.NewReader(body)), f.admin))
		codes[0] = rec.Code
	}()
	go func() {
		defer wg.Done()
		<-start
		codes[1] = f.approve(f.admin, pending, `{}`).Code
	}()
	close(start)
	wg.Wait()
	if codes[0] != http.StatusConflict || codes[1] != http.StatusOK {
		t.Fatalf("admin direct add and requester approval = %v, want 409 (other owner), 200 (approval)", codes)
	}
	book, err := f.books.GetByForeignID(context.Background(), "OL27482W")
	if err != nil || book == nil {
		t.Fatalf("book after race = %+v %v", book, err)
	}
	variants, err := db.NewBookVariantRepo(f.database).ListByBook(context.Background(), book.ID)
	if err != nil || len(variants) != 2 || variants[0].Language != "eng" || variants[1].Language != "rum" {
		t.Fatalf("race created duplicate variants: %+v %v", variants, err)
	}
	if n := countBookDownloads(t, f, book.ID); n != 0 {
		t.Fatalf("race started %d torrents", n)
	}
}

func TestRequestLanguagesOtherOwnersBookStaysConflict(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	owned := assertRequestLanguages(t, f, f.create(t, f.other, requestBookBody(`,"languages":["en"]`)), []string{"eng"})
	if rec := f.approve(f.admin, owned, `{}`); rec.Code != http.StatusOK {
		t.Fatalf("other's approval = %d %s", rec.Code, rec.Body.String())
	}
	rec := f.create(t, f.requester, requestBookBody(`,"languages":["ro"]`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("request for other's book = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if mine := f.listMine(t, f.requester); len(mine) != 0 {
		t.Fatalf("refusal left request: %+v", mine)
	}
}

func TestRequestLanguagesConcurrentApprovalOfMultipleLanguages(t *testing.T) {
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	created := assertRequestLanguages(t, f, f.create(t, f.requester, requestBookBody(`,"languages":["en","ro"]`)), []string{"eng", "rum"})
	codes := make([]int, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = f.approve(f.admin, created, `{}`).Code
		}(i)
	}
	close(start)
	wg.Wait()
	sort.Ints(codes)
	if !reflect.DeepEqual(codes, []int{http.StatusOK, http.StatusConflict}) {
		t.Fatalf("approvals = %v, want one 200 and one 409", codes)
	}
	book, err := f.books.GetByForeignID(context.Background(), "OL27482W")
	if err != nil || book == nil {
		t.Fatalf("approved book = %+v %v", book, err)
	}
	v, err := db.NewBookVariantRepo(f.database).ListByBook(context.Background(), book.ID)
	if err != nil || len(v) != 2 || v[0].Language != "eng" || v[1].Language != "rum" {
		t.Fatalf("variants = %+v %v", v, err)
	}
}

// An explicit ebook-only request must not make an audiobook in another
// language wanted just because the existing book also has an audio format.
func TestAddBookLanguagesRequestedFormatsOnly(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	post := func(media, language string) *httptest.ResponseRecorder {
		body := `{"foreignBookId":"OL27482W","foreignAuthorId":"OL39307A","mediaType":"` + media + `","languages":["` + language + `"]}`
		rec := httptest.NewRecorder()
		f.author.AddBook(rec, asUser(httptest.NewRequest(http.MethodPost, "/api/v1/author/book", strings.NewReader(body)), f.admin))
		return rec
	}
	if rec := post("both", "en"); rec.Code != http.StatusCreated {
		t.Fatalf("initial both formats = %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("ebook", "ro"); rec.Code != http.StatusCreated {
		t.Fatalf("extend ebook only = %d %s", rec.Code, rec.Body.String())
	}
	book, err := f.books.GetByForeignID(context.Background(), "OL27482W")
	if err != nil || book == nil {
		t.Fatalf("book after extension = %+v %v", book, err)
	}
	rows, err := db.NewBookVariantRepo(f.database).ListByBook(context.Background(), book.ID)
	if err != nil || len(rows) != 3 || rows[0].MediaType != "audiobook" || rows[0].Language != "eng" ||
		rows[1].MediaType != "ebook" || rows[1].Language != "eng" || rows[2].Language != "rum" {
		t.Fatalf("requested only Romanian ebook, got variants %+v %v", rows, err)
	}
}

func TestAddBookLanguagesDirectAndImportedExtension(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	post := func(user *db.User, languages string) *httptest.ResponseRecorder {
		t.Helper()
		body := `{"foreignBookId":"OL27482W","foreignAuthorId":"OL39307A","authorName":"H. G. Wells","mediaType":"ebook"` + languages + `}`
		rec := httptest.NewRecorder()
		f.author.AddBook(rec, asUser(httptest.NewRequest(http.MethodPost, "/api/v1/author/book", strings.NewReader(body)), user))
		return rec
	}
	for _, bad := range []string{`,"languages":null`, `,"languages":  null `, `,"languages":[]`, `,"languages":["de"]`} {
		rec := post(f.requester, bad)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid direct add %q = %d %s", bad, rec.Code, rec.Body.String())
		}
	}
	first := post(f.admin, `,"languages":["en"]`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first direct add = %d %s", first.Code, first.Body.String())
	}
	var book models.Book
	if err := json.Unmarshal(first.Body.Bytes(), &book); err != nil {
		t.Fatal(err)
	}
	v, err := db.NewBookVariantRepo(f.database).ListByBook(context.Background(), book.ID)
	if err != nil || len(v) != 1 || v[0].Language != "eng" {
		t.Fatalf("first direct add variants = %+v %v", v, err)
	}
	if err := f.books.AddBookFile(context.Background(), book.ID, "ebook", "/library/wells/book.epub"); err != nil {
		t.Fatal(err)
	}
	if rec := post(f.other, `,"languages":["ro"]`); rec.Code != http.StatusConflict {
		t.Fatalf("other user extending book = %d %s", rec.Code, rec.Body.String())
	}
	second := post(f.admin, `,"languages":["ro"]`)
	if second.Code != http.StatusCreated {
		t.Fatalf("extend imported book = %d %s", second.Code, second.Body.String())
	}
	var extended models.Book
	if err := json.Unmarshal(second.Body.Bytes(), &extended); err != nil {
		t.Fatal(err)
	}
	if extended.ID != book.ID {
		t.Fatalf("extension created book %d, want %d", extended.ID, book.ID)
	}
	v, err = db.NewBookVariantRepo(f.database).ListByBook(context.Background(), book.ID)
	if err != nil || len(v) != 2 || v[0].Language != "eng" || v[1].Language != "rum" {
		t.Fatalf("extended direct add variants = %+v %v", v, err)
	}
	if rec := post(f.admin, `,"languages":["ro"]`); rec.Code != http.StatusConflict {
		t.Fatalf("repeat direct add = %d %s, want 409", rec.Code, rec.Body.String())
	}
}
