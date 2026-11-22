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

// ReportError writes err in the shared error shape (stderr text; with
// asJSON also a ligus.error.v1 document on stdout) and returns the exit code.
func ReportError(stderr, stdout io.Writer, err error, cmd string, asJSON bool) int {
	return reportError(stderr, stdout, err, cmd, asJSON)
}

// WantsJSON reports whether a subcommand's arguments ask for JSON output.
func WantsJSON(args []string) bool { return wantsJSON(args) }

// Check runs `langstroth-relay check [--json] [--url URL]`, writing to w.
func Check(args []string, w io.Writer) error { return cmdCheck(args, w) }

// SplitFlagValues turns --flag=value into --flag value (before a "--"), so
// every command takes both spellings.
func SplitFlagValues(args []string) []string { return splitFlagValues(args) }
