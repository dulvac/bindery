package api

import (
	"github.com/vavallee/bindery/internal/db"
	"github.com/vavallee/bindery/internal/models"
)

// These are explicit allow-listed transport fields. In particular a
// requester never receives a BookFile, a filesystem path or a download URL.
type variantResponse struct {
	ID           int64  `json:"id"`
	MediaType    string `json:"mediaType"`
	Language     string `json:"language"`
	Status       string `json:"status"`
	Verification string `json:"verification"`
}

type legacyFileResponse struct {
	MediaType    string `json:"mediaType"`
	Language     string `json:"language"`
	Status       string `json:"status"`
	Verification string `json:"verification"`
}

func variantResponses(rows []db.BookVariantStatus) []variantResponse {
	out := make([]variantResponse, 0, len(rows))
	for _, v := range rows {
		out = append(out, variantResponse{ID: v.ID, MediaType: v.MediaType, Language: v.Language,
			Status: v.Status, Verification: v.Verification})
	}
	return out
}

func legacyFileResponses(rows []db.LegacyBookFileStatus) []legacyFileResponse {
	out := make([]legacyFileResponse, 0, len(rows))
	for _, f := range rows {
		out = append(out, legacyFileResponse{MediaType: f.MediaType, Status: models.VariantImported,
			Verification: "unverified"})
	}
	return out
}

func requestFormats(req models.LibraryRequest) []string {
	mediaType := req.MediaType
	if mediaType == "" {
		mediaType = req.BookMediaType
	}
	return models.RequestedMediaTypes(mediaType)
}

// Request status reports only the selected format/language pairs, rather
// than treating another copy of the same book as satisfying this request.
func requestedVariants(req models.LibraryRequest, rows []db.BookVariantStatus) []variantResponse {
	formats := make(map[string]bool)
	for _, format := range requestFormats(req) {
		formats[format] = true
	}
	languages := make(map[string]bool)
	for _, language := range req.Languages {
		languages[language] = true
	}
	out := make([]variantResponse, 0, len(rows))
	for _, v := range rows {
		if formats[v.MediaType] && languages[v.Language] {
			out = append(out, variantResponse{ID: v.ID, MediaType: v.MediaType, Language: v.Language,
				Status: v.Status, Verification: v.Verification})
		}
	}
	return out
}
