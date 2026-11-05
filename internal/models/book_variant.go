package models

import "slices"

// BookVariant is one requested language in one format of a book.
type BookVariant struct {
	ID        int64  `json:"id"`
	BookID    int64  `json:"bookId"`
	MediaType string `json:"mediaType"`
	Language  string `json:"language"`
	Status    string `json:"status"`
}

// NormalizeRequestLanguages returns unique, canonical language codes in
// lexical order. Requests without a language retain the existing English
// default, including when every supplied entry is blank.
func NormalizeRequestLanguages(languages []string) []string {
	unique := make(map[string]struct{}, len(languages))
	for _, language := range languages {
		if code := NormalizeLanguageCode(language); code != "" {
			unique[code] = struct{}{}
		}
	}
	if len(unique) == 0 {
		return []string{"eng"}
	}
	out := make([]string, 0, len(unique))
	for language := range unique {
		out = append(out, language)
	}
	slices.Sort(out)
	return out
}
