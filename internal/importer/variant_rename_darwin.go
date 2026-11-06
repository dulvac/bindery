//go:build darwin

package importer

import "golang.org/x/sys/unix"

func renameVariantDirNoReplace(fromFD int, from string, toFD int, to string) error {
	return unix.RenameatxNp(fromFD, from, toFD, to, unix.RENAME_EXCL)
}
