package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/vavallee/bindery/internal/auth"
)

// RetryReviewedVariant records an admin's deliberate decision to seek a new
// candidate for a reviewed copy. The rejected source and any torrent stay
// untouched; this route does not dispatch a search or call a download client.
func (h *BookHandler) RetryReviewedVariant(w http.ResponseWriter, r *http.Request) {
	if auth.UserRoleFromContext(r.Context()) != auth.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required"})
		return
	}
	book, ok := h.loadOwnedBook(w, r)
	if !ok {
		return
	}
	variantID, err := strconv.ParseInt(chi.URLParam(r, "variantId"), 10, 64)
	if err != nil || variantID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid variant id"})
		return
	}
	var input struct {
		Reason string `json:"reason"`
	}
	if err := decodeStrict(w, r, 4096, false, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid retry request"})
		return
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if input.Reason == "" || len(input.Reason) > 500 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reason is required (max 500 bytes)"})
		return
	}
	actor := auth.UserIDFromContext(r.Context())
	if actor <= 0 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "named admin required"})
		return
	}
	// Never allow a variant ID from another book, even when the actor is an
	// admin. A cross-book ID is opaque in exactly the way loadOwnedBook is.
	variants, err := h.books.VariantRepo().ListByBook(r.Context(), book.ID)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	found := false
	for _, v := range variants {
		if v.ID == variantID {
			found = true
			break
		}
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "variant not found"})
		return
	}
	changed, err := h.books.VariantRepo().RetryReviewed(r.Context(), book.ID, variantID, actor, input.Reason)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if !changed {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "variant is not in review or still has an import or download"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "wanted"})
}

// RetryReviewedVariantImport is a separate named admin decision to retry the
// old torrent, without releasing the reviewed copy to a wanted search. The
// DB switches both states and writes the audit in one transaction.
func (h *BookHandler) RetryReviewedVariantImport(w http.ResponseWriter, r *http.Request) {
	if auth.UserRoleFromContext(r.Context()) != auth.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required"})
		return
	}
	actor := auth.UserIDFromContext(r.Context())
	if actor <= 0 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "named admin required"})
		return
	}
	if h.downloads == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "download retry is not configured"})
		return
	}
	book, ok := h.loadOwnedBook(w, r)
	if !ok {
		return
	}
	variantID, err := strconv.ParseInt(chi.URLParam(r, "variantId"), 10, 64)
	if err != nil || variantID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid variant id"})
		return
	}
	var input struct {
		DownloadID int64   `json:"downloadId"`
		Reason     string  `json:"reason"`
		TorrentID  *string `json:"torrentId"`
	}
	if err := decodeStrict(w, r, 4096, false, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid retry request"})
		return
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if input.DownloadID <= 0 || input.Reason == "" || len(input.Reason) > 500 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "downloadId and reason are required (max 500 bytes)"})
		return
	}
	if input.TorrentID != nil && (strings.TrimSpace(*input.TorrentID) == "" || len(*input.TorrentID) > 128) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid torrent identity"})
		return
	}
	variants, err := h.books.VariantRepo().ListByBook(r.Context(), book.ID)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	found := false
	for _, v := range variants {
		if v.ID == variantID {
			found = true
			break
		}
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "variant not found"})
		return
	}
	dl, err := h.downloads.GetByID(r.Context(), input.DownloadID)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if dl == nil || dl.BookID == nil || *dl.BookID != book.ID || dl.VariantID == nil || *dl.VariantID != variantID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "download not found"})
		return
	}
	var changed bool
	if input.TorrentID != nil {
		changed, err = h.downloads.RetryReviewedImportMatchingTorrent(r.Context(), book.ID, variantID, input.DownloadID, actor, input.Reason, *input.TorrentID)
	} else {
		changed, err = h.downloads.RetryReviewedImport(r.Context(), book.ID, variantID, input.DownloadID, actor, input.Reason)
	}
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if !changed {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "variant or torrent is not available for a reviewed import retry"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "downloading"})
}
