package relay

import (
	"io"
)

// The command line lives in cmd/langstroth-relay; these are the pieces of
// it that need the relay's own settings and startup checks.

// Exit codes of the langstroth-relay binary.
const (
	ExitOK     = exitOK
	ExitFailed = exitFailed
	ExitUsage  = exitUsage
)

var (
	// ErrUsage marks a command-line mistake (exit 2).
	ErrUsage = errUsage
	// ErrChecksFailed is returned by Check when a check failed; the report
	// already said why.
	ErrChecksFailed = errChecksFailed
)

// UsageErrorf returns an error wrapping ErrUsage.
func UsageErrorf(format string, args ...any) error { return usageErrorf(format, args...) }

// Help writes `langstroth-relay help [topic]` to w.
func Help(args []string, w io.Writer) error { return cmdHelp(args, w) }

// WantsHelp reports whether a command's arguments ask for its help.
func WantsHelp(args []string) bool { return wantsHelp(args) }

// Check runs `langstroth-relay check [--json] [--url URL]`, writing to w.
func Check(args []string, w io.Writer) error { return cmdCheck(args, w) }
