package importer

import (
	"fmt"
	"path/filepath"
	"strings"
)

// VariantDestination names a new import without changing any tracked legacy
// path. The caller must still check for on-disk collisions before placement.
func VariantDestination(basePath, language string, folder bool) (string, error) {
	switch language {
	case "eng", "rum", "fre":
	default:
		return "", fmt.Errorf("unsupported variant language %q", language)
	}
	if !filepath.IsAbs(basePath) || filepath.Clean(basePath) != basePath || basePath == string(filepath.Separator) {
		return "", fmt.Errorf("invalid variant base path %q", basePath)
	}
	name := filepath.Base(basePath)
	if name == "." || name == ".." || name == "" {
		return "", fmt.Errorf("invalid variant base path %q", basePath)
	}
	if folder {
		return filepath.Join(filepath.Dir(basePath), name+" ["+language+"]"), nil
	}
	ext := filepath.Ext(name)
	if strings.HasSuffix(strings.ToLower(name), ".kepub.epub") {
		ext = name[len(name)-len(".kepub.epub"):]
	}
	if ext == "" || len(ext) == len(name) {
		return "", fmt.Errorf("variant ebook path has no extension: %q", basePath)
	}
	return filepath.Join(filepath.Dir(basePath), strings.TrimSuffix(name, ext)+" ["+language+"]"+ext), nil
}
