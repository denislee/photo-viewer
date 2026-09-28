//go:build !linux

package fsutil

func renameNoReplace(src, dst string) error { return errNoReplaceUnsupported }
