package models

import (
	"fmt"
	"slices"
	"strings"
)

const (
	VariantWanted      = "wanted"
	VariantDownloading = "downloading"
	VariantImported    = "imported"
	VariantReview      = "review"
)

// BookVariant is one requested language in one format of a book.
type BookVariant struct {
	ID        int64  `json:"id"`
	BookID    int64  `json:"bookId"`
	MediaType string `json:"mediaType"`
	Language  string `json:"language"`
	Status    string `json:"status"`
}

// NormalizeRequestLanguages returns unique, canonical request languages in
// lexical order. Only an absent selection defaults to English; an explicit
// empty or unsupported selection is an error.
func NormalizeRequestLanguages(languages []string) ([]string, error) {
	if languages == nil {
		return []string{"eng"}, nil
	}
	if len(languages) == 0 {
		return nil, fmt.Errorf("at least one request language is required")
	}
	unique := make(map[string]struct{}, len(languages))
	for _, language := range languages {
		var code string
		switch strings.ToLower(strings.TrimSpace(language)) {
		case "en", "eng":
			code = "eng"
		case "ro", "ron", "rum":
			code = "rum"
		case "fr", "fra", "fre":
			code = "fre"
		default:
			return nil, fmt.Errorf("invalid request language %q", language)
		}
		unique[code] = struct{}{}
	}
	out := make([]string, 0, len(unique))
	for language := range unique {
		out = append(out, language)
	}
	slices.Sort(out)
	return out, nil
}
