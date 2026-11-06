package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

// requesterLibraryBook is everything a requester may learn about a library
// book (security review item S1). It is a projection built field by field,
// not a copy of models.Book with fields blanked: a field added to models.Book
// later cannot reach a requester unless someone adds it here, and
// TestRequesterLibraryBook_FieldAllowList fails when they do.
//
// There are no file paths, no owner, no monitored flag and no download or
// file links. The add-language provider id is present only for owned books.
type requesterLibraryBook struct {
	ID             int64                `json:"id"`
	ForeignBookID  string               `json:"foreignBookId,omitempty"`
	Title          string               `json:"title"`
	AuthorName     string               `json:"authorName"`
	Series         string               `json:"series,omitempty"`
	SeriesPosition string               `json:"seriesPosition,omitempty"`
	CoverURL       string               `json:"coverUrl,omitempty"`
	Status         string               `json:"status"`
	Formats        []string             `json:"formats"`
	Variants       []variantResponse    `json:"variants"`
	LegacyFiles    []legacyFileResponse `json:"legacyFiles,omitempty"`
}

type requesterLibraryResponse struct {
	Items  []requesterLibraryBook `json:"items"`
	Total  int                    `json:"total"`
	Limit  int                    `json:"limit"`
	Offset int                    `json:"offset"`
}

// Library is the read only library browse for requesters.
// GET /requests/library?search=&limit=&offset=
//
// With tenancy off (the default) every user's books are listed, which is what
// the docs tell operators. With BINDERY_ENFORCE_TENANCY on, the list is scoped
// the way the Books page scopes it.
func (h *RequestHandler) Library(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, offset := parseLimitOffset(r, 60, 200)
	search := r.URL.Query().Get("search")
	if len(search) > 200 {
		search = search[:200]
	}
	scopeUserID := auth.ListScopeUserID(ctx)
	rows, total, err := h.requests.ListLibraryProjection(ctx, scopeUserID, strings.TrimSpace(search), limit, offset)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	items, err := h.projectLibrary(ctx, rows)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, requesterLibraryResponse{Items: items, Total: total, Limit: limit, Offset: offset})
}

func (h *RequestHandler) projectLibrary(ctx context.Context, rows []db.LibraryProjectionRow) ([]requesterLibraryBook, error) {
	scopeUserID := auth.ListScopeUserID(ctx)
	accesses := make([]db.BookStatusAccess, 0, len(rows))
	bookIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		accesses = append(accesses, db.BookStatusAccess{BookID: row.ID, OwnerUserID: scopeUserID})
		bookIDs = append(bookIDs, row.ID)
	}
	foreignIDs, err := h.requests.ListOwnedLibraryForeignIDs(ctx, auth.UserIDFromContext(ctx), bookIDs)
	if err != nil {
		return nil, err
	}
	statuses, err := h.variants.ListStatusByBooks(ctx, accesses)
	if err != nil {
		return nil, err
	}
	items := make([]requesterLibraryBook, 0, len(rows))
	for _, row := range rows {
		formats := make([]string, 0, 2)
		if row.HasEbook {
			formats = append(formats, models.MediaTypeEbook)
		}
		if row.HasAudiobook {
			formats = append(formats, models.MediaTypeAudiobook)
		}
		projection := statuses[db.BookStatusAccess{BookID: row.ID, OwnerUserID: scopeUserID}]
		items = append(items, requesterLibraryBook{
			ID:             row.ID,
			ForeignBookID:  foreignIDs[row.ID],
			Title:          row.Title,
			AuthorName:     row.AuthorName,
			Series:         row.SeriesTitle,
			SeriesPosition: row.SeriesPosition,
			CoverURL:       ProxyImageURL(row.ImageURL),
			Status:         row.Status,
			Formats:        formats,
			Variants:       variantResponses(projection.Variants),
			LegacyFiles:    legacyFileResponses(projection.LegacyFiles),
		})
	}
	return items, nil
}
