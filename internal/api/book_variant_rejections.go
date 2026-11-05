package api

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/vavallee/bindery/internal/downloader"
	"github.com/vavallee/bindery/internal/downloader/qbittorrent"
	"github.com/vavallee/bindery/internal/models"
	"github.com/vavallee/bindery/internal/pathmap"
)

// WithVariantRejections enables the administrative source-rejection endpoint.
// The only source roots accepted are the configured download directories; an
// empty configuration cannot grant access to an arbitrary filesystem path.
func (h *BookHandler) WithVariantRejections(repo *db.VariantRejectionRepo, downloadRoots ...string) *BookHandler {
	h.variantRejections = repo
	h.rejectionDownloadRoots = downloadRoots
	return h
}

// rejectionTorrentReader exposes only the read calls needed to prove an exact
// source. In tests it is replaced by a fake; the handler never calls a torrent
// mutator, even when the real client supports them.
type rejectionTorrentReader interface {
	GetTorrents(context.Context, string) ([]qbittorrent.Torrent, error)
	Files(context.Context, string) ([]qbittorrent.File, error)
}

// WithRejectionTorrentSource wires persisted client credentials and the same
// client-first/global-fallback path remapping used by the importer. Without
// these, the reject-source route fails closed rather than guessing a path.
func (h *BookHandler) WithRejectionTorrentSource(clients *db.DownloadClientRepo, globalRemap string) *BookHandler {
	h.rejectionClients = clients
	h.rejectionPathRemap = globalRemap
	h.rejectionTorrentReader = func(client *models.DownloadClient) rejectionTorrentReader {
		return downloader.QbittorrentFor(client)
	}
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
// Completed imports generally lack ImportPath. Instead the stored client and
// torrent hash select a live torrent whose file list proves each hardlink.
func (h *BookHandler) RejectVariantSource(w http.ResponseWriter, r *http.Request) {
	if auth.UserRoleFromContext(r.Context()) != auth.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required"})
		return
	}
	if h.variantRejections == nil || h.downloads == nil || h.roots == nil || h.rejectionClients == nil || h.rejectionTorrentReader == nil {
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
	source, err := h.proveRejectedTorrentSource(r.Context(), dl, file)
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

// proveRejectedTorrentSource reads only the stored client's torrent matching
// the stored hash. The file list, not a broad save directory or ImportPath,
// defines membership. Any unavailable or contradictory evidence is a 409.
func (h *BookHandler) proveRejectedTorrentSource(ctx context.Context, dl *models.Download, tracked *models.BookFile) (string, error) {
	if dl.Protocol != "torrent" || dl.DownloadClientID == nil || dl.TorrentID == nil || strings.TrimSpace(*dl.TorrentID) == "" {
		return "", errors.New("missing torrent identity")
	}
	client, err := h.rejectionClients.GetByID(ctx, *dl.DownloadClientID)
	if err != nil || client == nil || client.Type != "qbittorrent" || strings.TrimSpace(client.Username) == "" || client.Password == "" {
		return "", errors.New("qBittorrent client or credentials unavailable")
	}
	reader := h.rejectionTorrentReader(client)
	if reader == nil {
		return "", errors.New("qBittorrent reader unavailable")
	}
	torrents, err := reader.GetTorrents(ctx, "")
	if err != nil {
		return "", err
	}
	var torrent *qbittorrent.Torrent
	for i := range torrents {
		if strings.EqualFold(torrents[i].Hash, *dl.TorrentID) {
			if torrent != nil {
				return "", errors.New("ambiguous torrent identity")
			}
			torrent = &torrents[i]
		}
	}
	if torrent == nil {
		return "", errors.New("identified torrent not found")
	}
	files, err := reader.Files(ctx, torrent.Hash)
	if err != nil || len(files) == 0 {
		return "", errors.New("identified torrent has no readable file list")
	}
	return proveRejectedSourceFromTorrent(*torrent, files, client, h.rejectionPathRemap, tracked, h.rejectionDownloadRoots)
}

// proveRejectedSourceFromTorrent maps every file in the exact torrent through
// the importer path remap and refuses unsafe names, symlinks and paths outside
// the download roots before comparing device/inode with the library entry.
func proveRejectedSourceFromTorrent(torrent qbittorrent.Torrent, files []qbittorrent.File, client *models.DownloadClient, globalRemap string, tracked *models.BookFile, roots []string) (string, error) {
	remapper := pathmap.Parse(globalRemap)
	local := func(remote string) (string, error) {
		if !pathmap.IsAbsClientPath(remote) || hasRemoteDotDot(remote) || pathmap.CleanClientPath(remote) != remote {
			return "", errors.New("unsafe torrent path")
		}
		mapped, _ := downloader.RemapClientPath(client, remote, remapper)
		if !filepath.IsAbs(mapped) || filepath.Clean(mapped) != mapped || !pathHasNoSymlinks(mapped) {
			return "", errors.New("unmapped, noncanonical or symlinked torrent path")
		}
		return mapped, nil
	}
	if torrent.SavePath == "" || torrent.ContentPath == "" {
		return "", errors.New("torrent save or content path missing")
	}
	savePath, err := local(torrent.SavePath)
	if err != nil || !rejectionPathInRoots(savePath, roots, true) {
		return "", errors.New("torrent save path outside download roots")
	}
	contentPath, err := local(torrent.ContentPath)
	if err != nil || !rejectionPathInRoots(contentPath, roots, true) || !rejectionAtOrUnder(contentPath, savePath) {
		return "", errors.New("torrent content path outside save path or download roots")
	}
	contentInfo, err := os.Lstat(contentPath)
	if err != nil || (!contentInfo.IsDir() && !contentInfo.Mode().IsRegular()) {
		return "", errors.New("torrent content path missing")
	}
	listed := make(map[string]os.FileInfo, len(files))
	for _, f := range files {
		name := f.Name
		if name == "" || name == "." || pathmap.IsAbsClientPath(name) || strings.HasPrefix(name, `\`) || hasRemoteDotDot(name) || pathmap.CleanClientPath(name) != name {
			return "", errors.New("unsafe torrent file name")
		}
		path, err := local(pathmap.ClientPathJoin(torrent.SavePath, name))
		if err != nil || !rejectionPathInRoots(path, roots, false) || !rejectionAtOrUnder(path, savePath) || !rejectionAtOrUnder(path, contentPath) {
			return "", errors.New("torrent file outside its content path")
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return "", errors.New("torrent file missing or not regular")
		}
		if _, duplicate := listed[path]; duplicate {
			return "", errors.New("duplicate torrent file path")
		}
		listed[path] = info
	}
	switch tracked.Format {
	case models.MediaTypeEbook:
		lib, err := canonicalRegularFile(tracked.Path)
		if err != nil {
			return "", err
		}
		var match string
		for path, info := range listed {
			if os.SameFile(info, lib) {
				if match != "" || path == tracked.Path {
					return "", errors.New("ambiguous ebook source")
				}
				match = path
			}
		}
		if match == "" {
			return "", errors.New("ebook inode not in identified torrent")
		}
		// A second, unlisted hardlink in the torrent's own folder still
		// makes the claimed source ambiguous. Never scan a shared save root.
		if contentInfo.IsDir() && contentPath != savePath {
			found := 0
			err = filepath.WalkDir(contentPath, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.Type()&os.ModeSymlink != 0 {
					return errors.New("symlink in torrent content")
				}
				if entry.Type().IsRegular() {
					info, err := os.Lstat(path)
					if err != nil {
						return err
					}
					if os.SameFile(info, lib) {
						found++
					}
				}
				return nil
			})
			if err != nil || found != 1 {
				return "", errors.New("ambiguous hardlink in torrent content")
			}
		}
		return match, nil
	case models.MediaTypeAudiobook:
		libraryMedia, err := rejectionAudioMembers(tracked.Path)
		if err != nil {
			return "", err
		}
		var sourceMedia map[string]os.FileInfo
		if contentInfo.IsDir() {
			if contentPath == savePath || !rejectionPathInRoots(contentPath, roots, false) || contentPath == tracked.Path {
				return "", errors.New("audiobook has no distinct torrent folder")
			}
			sourceMedia, err = rejectionAudioMembers(contentPath)
			if err != nil {
				return "", err
			}
		} else {
			sourceMedia = map[string]os.FileInfo{contentPath: contentInfo}
		}
		listedAudio := make(map[string]os.FileInfo)
		for path, info := range listed {
			if models.IsAudioPath(path) {
				listedAudio[path] = info
			}
		}
		if len(listedAudio) != len(sourceMedia) || len(listedAudio) != len(libraryMedia) || len(listedAudio) == 0 {
			return "", errors.New("audiobook media membership incomplete")
		}
		for source, info := range listedAudio {
			if source == tracked.Path || sourceMedia[source] == nil {
				return "", errors.New("unlisted or self-referential audiobook source")
			}
			matches := 0
			for _, lib := range libraryMedia {
				if os.SameFile(info, lib) {
					matches++
				}
			}
			if matches != 1 {
				return "", errors.New("audiobook member inode mismatch or ambiguity")
			}
		}
		for _, lib := range libraryMedia {
			matches := 0
			for _, info := range listedAudio {
				if os.SameFile(info, lib) {
					matches++
				}
			}
			if matches != 1 {
				return "", errors.New("library audiobook member inode mismatch or ambiguity")
			}
		}
		return contentPath, nil
	default:
		return "", errors.New("unsupported tracked format")
	}
}

func rejectionAudioMembers(dir string) (map[string]os.FileInfo, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || !pathHasNoSymlinks(dir) {
		return nil, errors.New("audiobook directory is not canonical")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return nil, errors.New("tracked audiobook is not a directory")
	}
	members := make(map[string]os.FileInfo)
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in audiobook directory")
		}
		if models.IsAudioPath(path) {
			file, err := os.Lstat(path)
			if err != nil || !file.Mode().IsRegular() {
				return errors.New("audiobook member is not regular")
			}
			members[path] = file
		}
		return nil
	})
	if err != nil || len(members) == 0 {
		return nil, errors.New("audiobook has no safe media members")
	}
	return members, nil
}

func hasRemoteDotDot(path string) bool {
	for _, part := range strings.Split(strings.ReplaceAll(path, `\`, "/"), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func rejectionAtOrUnder(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func rejectionPathInRoots(path string, roots []string, allowRoot bool) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !pathHasNoSymlinks(path) {
		return false
	}
	for _, root := range roots {
		if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || !pathHasNoSymlinks(root) {
			continue
		}
		rootInfo, err := os.Lstat(root)
		if err != nil || !rootInfo.IsDir() {
			continue
		}
		if rel, err := filepath.Rel(root, path); err == nil && (allowRoot || rel != ".") && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
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
