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
