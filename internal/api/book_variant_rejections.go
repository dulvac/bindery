package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/vavallee/bindery/internal/auth"
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

// WithVariantRejections enables the administrative source-rejection endpoint.
// The only source roots accepted are the configured download directories; an
// empty configuration cannot grant access to an arbitrary filesystem path.
func (h *BookHandler) WithVariantRejections(repo *db.VariantRejectionRepo, downloadRoots ...string) *BookHandler {
	h.variantRejections = repo
	h.rejectionDownloadRoots = downloadRoots
	return h
}

// RegisterVariantRejectionRoute mounts the route under /api/v1. Keep the
// middleware at registration, even though the handler also checks the role.
func RegisterVariantRejectionRoute(r chi.Router, h *BookHandler) {
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAdmin)
		r.Post("/book/{id}/variants/{variantId}/reject-source", h.RejectVariantSource)
	})
}

type rejectVariantSourceRequest struct {
	DownloadID int64  `json:"downloadId"`
	BookFileID int64  `json:"bookFileId"`
	Reason     string `json:"reason"`
}

// RejectVariantSource records a no-reimport decision, not a cleanup action.
// An imported download usually lacks ImportPath (it was previously recorded
// only on failed imports). In that case the source is unprovable from the DB:
// return 409, never accept a guessed path from the request.
func (h *BookHandler) RejectVariantSource(w http.ResponseWriter, r *http.Request) {
	if auth.UserRoleFromContext(r.Context()) != auth.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required"})
		return
	}
	if h.variantRejections == nil || h.downloads == nil || h.roots == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "source rejection is not configured"})
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
	var input rejectVariantSourceRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields() // a supplied sourcePath must not silently be ignored
	if err := dec.Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid rejection request"})
		return
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid rejection request"})
		return
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if input.DownloadID <= 0 || input.BookFileID <= 0 || input.Reason == "" || len(input.Reason) > 500 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "downloadId, bookFileId and reason are required"})
		return
	}
	variants, err := h.books.VariantRepo().ListByBook(r.Context(), book.ID)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	var variant *models.BookVariant
	for i := range variants {
		if variants[i].ID == variantID {
			variant = &variants[i]
			break
		}
	}
	if variant == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "variant not found"})
		return
	}
	dl, err := h.downloads.GetByID(r.Context(), input.DownloadID)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	if dl == nil || dl.BookID == nil || *dl.BookID != book.ID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "download not found"})
		return
	}
	if dl.VariantID != nil && *dl.VariantID != variant.ID {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "download belongs to another variant"})
		return
	}
	files, err := h.books.ListBookFiles(r.Context(), book.ID)
	if err != nil {
		writeServerError(w, r, err)
		return
	}
	var file *models.BookFile
	for i := range files {
		if files[i].ID == input.BookFileID {
			file = &files[i]
			break
		}
	}
	if file == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "book file not found"})
		return
	}
	if file.Format != variant.MediaType || file.VariantID != nil && *file.VariantID != variant.ID {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "book file does not match variant"})
		return
	}
	if dl.CompletedAt == nil || dl.Status != models.StateImported && dl.Status != models.StateCompleted {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "download completion cannot be proven"})
		return
	}
	if !h.roots.Contains(r.Context(), file.Path) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "tracked file is outside the library"})
		return
	}
	source, err := proveRejectedSource(dl.ImportPath, file.Path, h.rejectionDownloadRoots)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "exact source provenance cannot be proven"})
		return
	}
	if err := h.variantRejections.Record(r.Context(), book.ID, variant.ID, dl.ID, source, input.Reason); err != nil {
		if errors.Is(err, db.ErrVariantRejectionAssociation) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "rejection ownership changed"})
			return
		}
		writeServerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

// proveRejectedSource looks only under a persisted download path. Matching
// inode/device to the tracked regular file is mandatory and exactly one path
// may match; a symlink, a sibling hardlink or an unknown root is ambiguous.
func proveRejectedSource(persistedPath, libraryPath string, roots []string) (string, error) {
	lib, err := canonicalRegularFile(libraryPath)
	if err != nil {
		return "", err
	}
	if persistedPath == "" || !filepath.IsAbs(persistedPath) || filepath.Clean(persistedPath) != persistedPath {
		return "", errors.New("download path was not persisted as an absolute path")
	}
	allowed := false
	for _, root := range roots {
		if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
			continue
		}
		rootInfo, err := os.Lstat(root)
		if err != nil || !rootInfo.IsDir() || !pathHasNoSymlinks(root) {
			continue
		}
		rel, err := filepath.Rel(root, persistedPath)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			allowed = true
			break
		}
	}
	if !allowed || !pathHasNoSymlinks(persistedPath) {
		return "", errors.New("download path is not a real path under a configured root")
	}
	info, err := os.Lstat(persistedPath)
	if err != nil {
		return "", err
	}
	if info.Mode().IsRegular() {
		if persistedPath == libraryPath || !os.SameFile(info, lib) {
			return "", errors.New("download and library files have different inodes")
		}
		return persistedPath, nil
	}
	if !info.IsDir() {
		return "", errors.New("download path is not a file or directory")
	}
	var match string
	err = filepath.WalkDir(persistedPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in download directory")
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		candidate, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if os.SameFile(candidate, lib) {
			if path == libraryPath {
				return errors.New("source is the tracked library path")
			}
			if match != "" {
				return fmt.Errorf("multiple source paths match tracked file")
			}
			match = path
		}
		return nil
	})
	if err != nil || match == "" {
		return "", errors.New("no unique matching regular source file under persisted download path")
	}
	return match, nil
}

func canonicalRegularFile(path string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !pathHasNoSymlinks(path) {
		return nil, errors.New("file path is relative or symlinked")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("tracked file is not regular")
	}
	return info, nil
}

func pathHasNoSymlinks(path string) bool {
	real, err := filepath.EvalSymlinks(path)
	return err == nil && real == path
}
