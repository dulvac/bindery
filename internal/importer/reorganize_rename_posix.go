//go:build linux || darwin

package importer

import "golang.org/x/sys/unix"

// A rename with NOREPLACE makes the collision check at commit authoritative.
// A cross-device move fails rather than falling back to a copy of a hardlink.
func reorganizeRenameNoReplace(source, dest string) error {
	return renameVariantDirNoReplace(unix.AT_FDCWD, source, unix.AT_FDCWD, dest)
}
