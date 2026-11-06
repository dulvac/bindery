package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

// A projection without the file join (or one inferred from books.language)
// loses the distinction between an imported English copy and an old file.
func TestVariantResponseBookDetailAndOwnerIsolation(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newRequestsFixture(t, wellsRequestStub(), &fakeAdder{})
	ctx := context.Background()
	author := seedWells(t, f.authors)
	book := &models.Book{ForeignID: "OL-VARIANTS", AuthorID: author.ID, OwnerUserID: f.requester.ID,
		Title: "Variants", SortTitle: "variants", Language: "eng", MediaType: models.MediaTypeBoth, Status: models.BookStatusImported}
	if err := f.books.Create(ctx, book); err != nil {
		t.Fatal(err)
	}
	copies, err := f.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	audio, err := f.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeAudiobook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.books.VariantRepo().SetStatus(ctx, copies[0].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	if err := f.books.VariantRepo().SetStatus(ctx, audio[0].ID, models.VariantReview); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		format, path, language, verification string
		variantID                            any
	}{
		{"ebook", "/secret/verified-eng.epub", "eng", "verified", copies[0].ID},
		{"ebook", "/secret/old-book.epub", "", "unverified", nil},
	} {
		if _, err := f.database.ExecContext(ctx, `INSERT INTO book_files (book_id, format, path, variant_id, language, verification) VALUES (?, ?, ?, ?, ?, ?)`,
			book.ID, row.format, row.path, row.variantID, row.language, row.verification); err != nil {
			t.Fatal(err)
		}
	}
	h := NewBookHandler(f.books, nil, db.NewHistoryRepo(f.database), nil)
	get := func(userID int64) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		r := withURLParam(httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/book/%d", book.ID), nil), "id", strconv.FormatInt(book.ID, 10))
		h.Get(rec, r.WithContext(auth.WithUserRole(auth.WithUserID(r.Context(), userID), auth.RoleUser)))
		return rec
	}
	if rec := get(f.other.ID); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "Variants") {
		t.Fatalf("other owner read = %d %s, want opaque 404", rec.Code, rec.Body.String())
	}
	rec := get(f.requester.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("own detail = %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Language  string `json:"language"`
		BookFiles []struct {
			Path string `json:"path"`
		} `json:"bookFiles"`
		Variants []struct {
			ID           int64  `json:"id"`
			MediaType    string `json:"mediaType"`
			Language     string `json:"language"`
			Status       string `json:"status"`
			Verification string `json:"verification"`
		} `json:"variants"`
		LegacyFiles []struct {
			MediaType    string `json:"mediaType"`
			Language     string `json:"language"`
			Status       string `json:"status"`
			Verification string `json:"verification"`
		} `json:"legacyFiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Language != "eng" || len(got.BookFiles) != 2 || got.BookFiles[0].Path == "" {
		t.Fatalf("legacy admin/user fields disappeared: %+v", got)
	}
	want := []struct {
		id                                int64
		media, lang, status, verification string
	}{
		{audio[0].ID, "audiobook", "eng", "review", ""},
		{copies[0].ID, "ebook", "eng", "imported", "verified"},
		{copies[1].ID, "ebook", "rum", "wanted", ""},
	}
	if len(got.Variants) != len(want) {
		t.Fatalf("detail variants = %+v, want %+v", got.Variants, want)
	}
	for i, v := range got.Variants {
		if v.ID != want[i].id || v.MediaType != want[i].media || v.Language != want[i].lang || v.Status != want[i].status || v.Verification != want[i].verification {
			t.Fatalf("variant %d = %+v, want %+v", i, v, want[i])
		}
	}
	if len(got.LegacyFiles) != 1 || got.LegacyFiles[0].MediaType != "ebook" || got.LegacyFiles[0].Language != "" ||
		got.LegacyFiles[0].Status != "imported" || got.LegacyFiles[0].Verification != "unverified" {
		t.Fatalf("old book file must be separate, never labelled by book language: %+v", got.LegacyFiles)
	}
}

// A missing requested copy cannot be substituted by another format or the
// catalogue's book-level imported status. An unverified linked file is still
// available, with its evidence marked honestly.
func TestVariantFulfilledRequiresEverySelectedCopy(t *testing.T) {
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	created := decodeRequest(t, f.create(t, f.requester, `{"kind":"book","foreignId":"OL27482W","mediaType":"both","languages":["eng","rum"]}`))
	approved := f.approve(f.admin, created.ID, `{"searchOnAdd":false}`)
	if approved.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", approved.Code, approved.Body.String())
	}
	book, err := f.books.GetByForeignID(context.Background(), "OL27482W")
	if err != nil || book == nil {
		t.Fatalf("approved book: %+v %v", book, err)
	}
	variants, err := f.books.VariantRepo().ListByBook(context.Background(), book.ID)
	if err != nil || len(variants) != 4 {
		t.Fatalf("all requested variants: %+v %v", variants, err)
	}
	var engEbook, rumEbook, engAudio, rumAudio int64
	for _, v := range variants {
		switch v.MediaType + "/" + v.Language {
		case "ebook/eng":
			engEbook = v.ID
		case "ebook/rum":
			rumEbook = v.ID
		case "audiobook/eng":
			engAudio = v.ID
		case "audiobook/rum":
			rumAudio = v.ID
		}
	}
	if engEbook*rumEbook*engAudio*rumAudio == 0 {
		t.Fatalf("missing format/language variant: %+v", variants)
	}
	set := func(id int64, status string) {
		t.Helper()
		if err := f.books.VariantRepo().SetStatus(context.Background(), id, status); err != nil {
			t.Fatal(err)
		}
	}
	set(engEbook, models.VariantImported)
	set(rumEbook, models.VariantWanted)
	set(engAudio, models.VariantReview)
	// Use a real file association for the final copy; the unverified status
	// should not turn into a claim that the file's language was verified.
	set(rumAudio, models.VariantImported)
	if _, err := f.database.Exec(`INSERT INTO book_files (book_id, format, path, variant_id, language, verification) VALUES (?, 'audiobook', '/secret/unknown-audio', ?, '', 'unverified')`, book.ID, rumAudio); err != nil {
		t.Fatal(err)
	}
	check := func(wantFulfilled bool) {
		t.Helper()
		for _, caller := range []struct {
			name string
			call func(*httptest.ResponseRecorder)
		}{
			{"mine", func(rec *httptest.ResponseRecorder) {
				f.h.ListMine(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/v1/requests", nil), f.requester))
			}},
			{"queue", func(rec *httptest.ResponseRecorder) {
				f.h.Queue(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/v1/requests/queue?status=approved", nil), f.admin))
			}},
		} {
			rec := httptest.NewRecorder()
			caller.call(rec)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", caller.name, rec.Code, rec.Body.String())
			}
			var list struct {
				Items []struct {
					Fulfilled bool `json:"fulfilled"`
					Variants  []struct {
						ID           int64  `json:"id"`
						MediaType    string `json:"mediaType"`
						Language     string `json:"language"`
						Status       string `json:"status"`
						Verification string `json:"verification"`
					} `json:"variants"`
				} `json:"items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 1 || list.Items[0].Fulfilled != wantFulfilled || len(list.Items[0].Variants) != 4 {
				t.Fatalf("%s = %+v, want fulfilled=%v and four variants: %s", caller.name, list, wantFulfilled, rec.Body.String())
			}
			found := false
			for _, v := range list.Items[0].Variants {
				if v.ID == rumAudio && v.MediaType == "audiobook" && v.Language == "rum" && v.Status == "imported" && v.Verification == "unverified" {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s failed to mark linked unknown-language copy unverified: %s", caller.name, rec.Body.String())
			}
		}
	}
	check(false)
	set(rumEbook, models.VariantImported)
	check(false) // English audio is still under review.
	set(engAudio, models.VariantImported)
	check(true)
}

// Pending selections must remain visible before approval, including an
// already-imported copy on the requester's own book. Missing pairs cannot
// silently disappear just because they have no book_variants row yet.
func TestVariantPendingRequestProjectsSelectedPairsAndOwnedBookStatuses(t *testing.T) {
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	ctx := context.Background()
	created := decodeRequest(t, f.create(t, f.requester, `{"kind":"book","foreignId":"OL27482W","mediaType":"ebook","languages":["eng"]}`))
	if rec := f.approve(f.admin, created.ID, `{"searchOnAdd":false}`); rec.Code != http.StatusOK {
		t.Fatalf("approve English: %d %s", rec.Code, rec.Body.String())
	}
	book, err := f.books.GetByForeignID(ctx, "OL27482W")
	if err != nil || book == nil {
		t.Fatalf("approved book: %+v %v", book, err)
	}
	english, err := f.books.VariantRepo().ListByBook(ctx, book.ID)
	if err != nil || len(english) != 1 {
		t.Fatalf("English variant: %+v %v", english, err)
	}
	if err := f.books.VariantRepo().SetStatus(ctx, english[0].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	pending := decodeRequest(t, f.create(t, f.requester, `{"kind":"book","foreignId":"OL27482W","mediaType":"both","languages":["eng","rum"]}`))
	if pending.Status != models.RequestStatusPending {
		t.Fatalf("request after extending existing book: %+v", pending)
	}
	rec := httptest.NewRecorder()
	f.h.ListMine(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/v1/requests", nil), f.requester))
	if rec.Code != http.StatusOK {
		t.Fatalf("mine: %d %s", rec.Code, rec.Body.String())
	}
	var got requestListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].Fulfilled {
		t.Fatalf("pending request must be unfulfilled: %+v", got.Items)
	}
	pairs := map[string]string{}
	for _, v := range got.Items[0].Variants {
		pairs[v.MediaType+"/"+v.Language] = v.Status
	}
	want := map[string]string{"ebook/eng": "imported", "ebook/rum": "wanted", "audiobook/eng": "wanted", "audiobook/rum": "wanted"}
	if !reflect.DeepEqual(pairs, want) || len(got.Items[0].Variants) != len(want) {
		t.Fatalf("pending requested copies = %v, want %v: %s", pairs, want, rec.Body.String())
	}
}

// An approved request must not keep showing another owner's statuses if its
// result book is reassigned after approval (even before tenancy is enabled).
func TestVariantFulfilledOwnerReassignmentDoesNotExposeCopies(t *testing.T) {
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	created := decodeRequest(t, f.create(t, f.requester, `{"kind":"book","foreignId":"OL27482W","mediaType":"ebook"}`))
	if rec := f.approve(f.admin, created.ID, `{"searchOnAdd":false}`); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	book, err := f.books.GetByForeignID(context.Background(), "OL27482W")
	if err != nil || book == nil {
		t.Fatalf("approved book: %+v %v", book, err)
	}
	copies, err := f.books.VariantRepo().ListByBook(context.Background(), book.ID)
	if err != nil || len(copies) != 1 {
		t.Fatalf("approved copy: %+v %v", copies, err)
	}
	if err := f.books.VariantRepo().SetStatus(context.Background(), copies[0].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.Exec(`UPDATE books SET owner_user_id = ? WHERE id = ?`, f.other.ID, book.ID); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.h.ListMine(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/v1/requests", nil), f.requester))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner list: %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Items []struct {
			Fulfilled bool  `json:"fulfilled"`
			Variants  []any `json:"variants"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].Fulfilled || len(got.Items[0].Variants) != 0 {
		t.Fatalf("reassigned book leaked its new owner's status: %s", rec.Body.String())
	}
}

// Pre-variant request rows can lack a media type; the existing fulfillment
// path falls back to the approved book's format. The status view must agree.
func TestVariantResponseLegacyRequestFormatFallsBackToBook(t *testing.T) {
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	created := decodeRequest(t, f.create(t, f.requester, `{"kind":"book","foreignId":"OL27482W","mediaType":"ebook"}`))
	if rec := f.approve(f.admin, created.ID, `{"searchOnAdd":false}`); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	book, err := f.books.GetByForeignID(context.Background(), "OL27482W")
	if err != nil || book == nil {
		t.Fatalf("approved book: %+v %v", book, err)
	}
	copies, err := f.books.VariantRepo().ListByBook(context.Background(), book.ID)
	if err != nil || len(copies) != 1 {
		t.Fatalf("approved variant: %+v %v", copies, err)
	}
	if err := f.books.VariantRepo().SetStatus(context.Background(), copies[0].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.Exec(`UPDATE requests SET media_type = '' WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.h.ListMine(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/v1/requests", nil), f.requester))
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy request: %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Items []struct {
			Fulfilled bool `json:"fulfilled"`
			Variants  []struct {
				ID        int64  `json:"id"`
				MediaType string `json:"mediaType"`
			} `json:"variants"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || !got.Items[0].Fulfilled || len(got.Items[0].Variants) != 1 || got.Items[0].Variants[0].ID != copies[0].ID || got.Items[0].Variants[0].MediaType != "ebook" {
		t.Fatalf("legacy request inconsistent status: %s", rec.Body.String())
	}
}

func TestVariantResponseRequesterLibraryPagesAndNeverExposesPaths(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newRequestsFixture(t, wellsRequestStub(), &fakeAdder{})
	ctx := context.Background()
	author := seedWells(t, f.authors)
	makeBook := func(title string, owner int64) *models.Book {
		t.Helper()
		b := &models.Book{ForeignID: "OL-" + title, AuthorID: author.ID, OwnerUserID: owner, Title: title,
			SortTitle: strings.ToLower(title), Language: "eng", MediaType: models.MediaTypeEbook, Status: models.BookStatusImported}
		if err := f.books.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	alpha := makeBook("Alpha", f.requester.ID)
	beta := makeBook("Beta", f.requester.ID)
	_ = makeBook("Other", f.other.ID)
	v, err := f.books.VariantRepo().EnsureWanted(ctx, alpha.ID, models.MediaTypeEbook, []string{"eng", "rum"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.books.VariantRepo().SetStatus(ctx, v[0].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		book               int64
		path               string
		variant            any
		lang, verification string
	}{
		{alpha.ID, "/private/book-alpha-eng.epub", v[0].ID, "eng", "verified"},
		{beta.ID, "/private/book-beta-legacy.epub", nil, "", "unverified"},
	} {
		if _, err := f.database.ExecContext(ctx, `INSERT INTO book_files (book_id, format, path, variant_id, language, verification) VALUES (?, 'ebook', ?, ?, ?, ?)`, row.book, row.path, row.variant, row.lang, row.verification); err != nil {
			t.Fatal(err)
		}
	}
	for page, title := range []string{"Alpha", "Beta"} {
		rec := httptest.NewRecorder()
		f.h.Library(rec, asUser(httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/requests/library?limit=1&offset=%d", page), nil), f.requester))
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, key := range []string{"/private/", "path", "filePath", "ebookFilePath", "audiobookFilePath", "downloadUrl", "torrent", "OL-Other"} {
			if strings.Contains(strings.ToLower(body), strings.ToLower(key)) {
				t.Errorf("page %d leaks %s: %s", page, key, body)
			}
		}
		var result struct {
			Total  int `json:"total"`
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
			Items  []struct {
				Title         string `json:"title"`
				ForeignBookID string `json:"foreignBookId"`
				Variants      []struct {
					ID           int64  `json:"id"`
					MediaType    string `json:"mediaType"`
					Language     string `json:"language"`
					Status       string `json:"status"`
					Verification string `json:"verification"`
				} `json:"variants"`
				LegacyFiles []struct {
					MediaType    string `json:"mediaType"`
					Language     string `json:"language"`
					Verification string `json:"verification"`
				} `json:"legacyFiles"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Total != 2 || result.Limit != 1 || result.Offset != page || len(result.Items) != 1 || result.Items[0].Title != title || result.Items[0].ForeignBookID != "OL-"+title {
			t.Fatalf("page %d = %+v, want owned book %s with its add id", page, result, title)
		}
		if page == 0 {
			want := []string{"ebook/eng/imported/verified", "ebook/rum/wanted/"}
			got := make([]string, 0, len(result.Items[0].Variants))
			for _, v := range result.Items[0].Variants {
				got = append(got, fmt.Sprintf("%s/%s/%s/%s", v.MediaType, v.Language, v.Status, v.Verification))
			}
			if !reflect.DeepEqual(got, want) || result.Items[0].Variants[0].ID != v[0].ID {
				t.Fatalf("Alpha variants = %v, want %v", got, want)
			}
		} else if len(result.Items[0].Variants) != 0 || len(result.Items[0].LegacyFiles) != 1 || result.Items[0].LegacyFiles[0].Language != "" || result.Items[0].LegacyFiles[0].Verification != "unverified" {
			t.Fatalf("Beta legacy entry was inferred from catalogue English: %+v", result.Items[0])
		}
	}
}

// A book can change owners after the library page is selected but before
// its variants and legacy files are read. Only the current owner may see them.
func TestVariantRequesterLibraryReassignmentBetweenReads(t *testing.T) {
	auth.SetEnforceTenancyForTests(t, true)
	f := newRequestsFixture(t, wellsRequestStub(), &fakeAdder{})
	ctx := asUser(httptest.NewRequest(http.MethodGet, "/api/v1/requests/library", nil), f.requester).Context()
	author := seedWells(t, f.authors)
	book := &models.Book{ForeignID: "OL-LIB-RACE", AuthorID: author.ID, OwnerUserID: f.requester.ID,
		Title: "Reassigned", SortTitle: "reassigned", MediaType: models.MediaTypeEbook, Status: models.BookStatusImported}
	if err := f.books.Create(ctx, book); err != nil {
		t.Fatal(err)
	}
	copies, err := f.books.VariantRepo().EnsureWanted(ctx, book.ID, models.MediaTypeEbook, []string{"eng"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.books.VariantRepo().SetStatus(ctx, copies[0].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	const path = "/private/new-owner-legacy.epub"
	if _, err := f.database.ExecContext(ctx, `INSERT INTO book_files (book_id, format, path) VALUES (?, 'ebook', ?)`, book.ID, path); err != nil {
		t.Fatal(err)
	}
	rows, total, err := f.requests.ListLibraryProjection(ctx, auth.ListScopeUserID(ctx), "", 10, 0)
	if err != nil || total != 1 || len(rows) != 1 || rows[0].ID != book.ID {
		t.Fatalf("authorized library page: rows=%+v total=%d err=%v", rows, total, err)
	}
	if _, err := f.database.ExecContext(ctx, `UPDATE books SET owner_user_id = ? WHERE id = ?`, f.other.ID, book.ID); err != nil {
		t.Fatal(err)
	}
	items, err := f.h.projectLibrary(ctx, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0].Variants) != 0 || len(items[0].LegacyFiles) != 0 || items[0].ForeignBookID != "" {
		t.Fatalf("old owner's paginated book exposed new owner's status or add id: %+v", items)
	}
	payload, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), path) || strings.Contains(string(payload), "path") {
		t.Fatalf("requester library exposed legacy file path: %s", payload)
	}
	// The new owner still sees the copies, but never a filesystem path.
	otherCtx := asUser(httptest.NewRequest(http.MethodGet, "/api/v1/requests/library", nil), f.other).Context()
	owned, err := f.h.projectLibrary(otherCtx, rows)
	if err != nil || len(owned) != 1 || len(owned[0].Variants) != 1 || len(owned[0].LegacyFiles) != 1 || owned[0].ForeignBookID != "OL-LIB-RACE" {
		t.Fatalf("new owner status and add id: items=%+v err=%v", owned, err)
	}
	ownedJSON, err := json.Marshal(owned)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ownedJSON), path) || strings.Contains(string(ownedJSON), "path") {
		t.Fatalf("new owner's library exposed legacy file path: %s", ownedJSON)
	}
}

// An admin's forUser queue must project the target's copies, not the admin's.
// A stale request page must stop showing them after the book changes owners.
func TestVariantRequestReassignmentBetweenReads(t *testing.T) {
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	created := decodeRequest(t, f.create(t, f.admin, forUserBody("OL27482W", f.requester.ID)))
	if rec := f.approve(f.admin, created.ID, `{"searchOnAdd":false}`); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	ctx := context.Background()
	book, err := f.books.GetByForeignID(ctx, "OL27482W")
	if err != nil || book == nil {
		t.Fatalf("approved book: %+v %v", book, err)
	}
	copies, err := f.books.VariantRepo().ListByBook(ctx, book.ID)
	if err != nil || len(copies) != 1 {
		t.Fatalf("requested copy: %+v %v", copies, err)
	}
	if err := f.books.VariantRepo().SetStatus(ctx, copies[0].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	const path = "/private/new-owner-legacy.epub"
	if _, err := f.database.ExecContext(ctx, `INSERT INTO book_files (book_id, format, path) VALUES (?, 'ebook', ?)`, book.ID, path); err != nil {
		t.Fatal(err)
	}
	mine, total, err := f.requests.ListByOwner(ctx, f.requester.ID, 10, 0)
	if err != nil || total != 1 || len(mine) != 1 || !mine[0].BookFulfilled {
		t.Fatalf("first owner read: requests=%+v total=%d err=%v", mine, total, err)
	}
	queue, total, err := f.requests.ListAll(ctx, models.RequestStatusApproved, 10, 0)
	if err != nil || total != 1 || len(queue) != 1 || !queue[0].BookFulfilled {
		t.Fatalf("admin forUser read: requests=%+v total=%d err=%v", queue, total, err)
	}
	visible, err := f.h.projectRequests(ctx, queue, true)
	if err != nil || len(visible) != 1 || visible[0].Username != f.requester.Username ||
		visible[0].CreatedVia != models.RequestCreatedViaAdmin || !visible[0].Fulfilled || len(visible[0].Variants) != 1 || len(visible[0].LegacyFiles) != 1 {
		t.Fatalf("admin queue forUser target status: responses=%+v err=%v", visible, err)
	}
	if _, err := f.database.ExecContext(ctx, `UPDATE books SET owner_user_id = ? WHERE id = ?`, f.other.ID, book.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		items []models.LibraryRequest
		admin bool
	}{
		{"own requests", mine, false},
		{"admin forUser queue", queue, true},
	} {
		out, err := f.h.projectRequests(ctx, tc.items, tc.admin)
		if err != nil || len(out) != 1 {
			t.Fatalf("%s projection: responses=%+v err=%v", tc.name, out, err)
		}
		if out[0].Fulfilled || len(out[0].Variants) != 0 || len(out[0].LegacyFiles) != 0 {
			t.Fatalf("%s leaked new owner's statuses after reassignment: %+v", tc.name, out[0])
		}
		payload, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(payload), path) || strings.Contains(string(payload), "path") {
			t.Fatalf("%s leaked legacy file path: %s", tc.name, payload)
		}
	}
}

// Two approved requests may reference the same old unowned book. After a
// reassignment, the admin queue must use each request's owner separately.
func TestVariantAdminQueueSharedBookScopesEachRequestOwner(t *testing.T) {
	f := newRequestsFixture(t, wellsRequestStub(), nil)
	first := decodeRequest(t, f.create(t, f.admin, forUserBody("OL27482W", f.requester.ID)))
	second := decodeRequest(t, f.create(t, f.other, `{"kind":"book","foreignId":"OL27482W","mediaType":"ebook"}`))
	if rec := f.approve(f.admin, first.ID, `{"searchOnAdd":false}`); rec.Code != http.StatusOK {
		t.Fatalf("approve first: %d %s", rec.Code, rec.Body.String())
	}
	ctx := context.Background()
	book, err := f.books.GetByForeignID(ctx, "OL27482W")
	if err != nil || book == nil {
		t.Fatalf("book: %+v %v", book, err)
	}
	copies, err := f.books.VariantRepo().ListByBook(ctx, book.ID)
	if err != nil || len(copies) != 1 {
		t.Fatalf("book variant: %+v %v", copies, err)
	}
	if err := f.books.VariantRepo().SetStatus(ctx, copies[0].ID, models.VariantImported); err != nil {
		t.Fatal(err)
	}
	claim, err := f.requests.Claim(ctx, second.ID, f.admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.requests.Complete(ctx, second.ID, claim.ClaimToken, &book.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.ExecContext(ctx, `UPDATE books SET owner_user_id = NULL WHERE id = ?`, book.ID); err != nil {
		t.Fatal(err)
	}
	queue, total, err := f.requests.ListAll(ctx, models.RequestStatusApproved, 10, 0)
	if err != nil || total != 2 || len(queue) != 2 || !queue[0].BookFulfilled || !queue[1].BookFulfilled {
		t.Fatalf("unowned book queue: rows=%+v total=%d err=%v", queue, total, err)
	}
	if _, err := f.database.ExecContext(ctx, `UPDATE books SET owner_user_id = ? WHERE id = ?`, f.other.ID, book.ID); err != nil {
		t.Fatal(err)
	}
	out, err := f.h.projectRequests(ctx, queue, true)
	if err != nil {
		t.Fatal(err)
	}
	byUser := make(map[string]requestResponse, len(out))
	for _, row := range out {
		byUser[row.Username] = row
	}
	if old := byUser[f.requester.Username]; old.Fulfilled || len(old.Variants) != 0 {
		t.Fatalf("old owner got new owner's variant: %+v", old)
	}
	if current := byUser[f.other.Username]; !current.Fulfilled || len(current.Variants) != 1 || current.Variants[0].ID != copies[0].ID {
		t.Fatalf("new owner lost variant in mixed admin queue: %+v", current)
	}
}
