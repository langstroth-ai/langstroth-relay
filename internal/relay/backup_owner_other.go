//go:build !unix

package relay

import (
	"io/fs"
	"os"
)

// ownedByMe can't check ownership here; the mode checks still apply.
func ownedByMe(fs.FileInfo) bool { return true }

// openNoFollow opens path read-only (the Lstat check before it refuses a
// symlink; os.SameFile catches a swap).
func openNoFollow(path string) (*os.File, error) { return os.Open(path) }
