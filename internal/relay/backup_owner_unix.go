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
