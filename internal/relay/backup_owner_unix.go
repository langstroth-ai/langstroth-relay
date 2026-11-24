//go:build unix

package relay

import (
	"io/fs"
	"os"
	"syscall"
)

// ownedByMe reports whether fi belongs to this process's effective user.
func ownedByMe(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

// openNoFollow opens path read-only, refusing a symlink.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
