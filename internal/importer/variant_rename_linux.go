//go:build linux

package importer

import "golang.org/x/sys/unix"

func renameVariantDirNoReplace(fromFD int, from string, toFD int, to string) error {
	return unix.Renameat2(fromFD, from, toFD, to, unix.RENAME_NOREPLACE)
}
