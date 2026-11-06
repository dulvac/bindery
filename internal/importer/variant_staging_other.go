//go:build !linux && !darwin

package importer

import (
	"context"
	"fmt"

	"github.com/vavallee/bindery/internal/models"
)

// No fallback may copy or move a torrent source, or rename an audiobook
// folder on a platform without a known atomic no-replace primitive.
func (s *Scanner) importTorrentVariant(_ context.Context, _ *models.Download, _ *models.Book, _ *models.Author,
	_ string, _ string, _ []string, _ []checkedVariantSource) error {
	return fmt.Errorf("variant placement has no safe no-replace promotion on this platform")
}
