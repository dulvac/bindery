//go:build !linux && !darwin

package importer

import "fmt"

// A platform without an atomic no-replace rename must not risk overwriting
// an occupied library destination or copying a seeded hardlink across disks.
func reorganizeRenameNoReplace(_, _ string) error {
	return fmt.Errorf("reorganize requires atomic no-replace rename on this platform")
}
