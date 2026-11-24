//go:build !unix

package relay

import "io/fs"

// ownedByMe can't check ownership here; the mode checks still apply.
func ownedByMe(fs.FileInfo) bool { return true }
