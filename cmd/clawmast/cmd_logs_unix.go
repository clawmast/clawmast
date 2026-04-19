//go:build unix

package main

import (
	"os"
	"syscall"
)

// inodeOf returns a stable file identity on Unix hosts. Used by
// followFile to detect log rotation (launchd rename, systemd +
// logrotate) by comparing the active inode against the currently-open
// one. Returns 0 on any error; 0 compares unequal to any real inode so
// we conservatively re-open in that case.
func inodeOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
