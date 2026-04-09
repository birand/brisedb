//go:build !linux

package brisedb

import "os"

// newFileReader returns a posixFile on non-Linux platforms.
func newFileReader(f *os.File) fileReader { return &posixFile{f} }
