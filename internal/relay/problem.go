package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Errors on the command line share one shape with every Ligus tool
// (`ligus help exit-codes`): the text is "langstroth-relay: <what>", then an
// indented "why:" and "fix:" line when there is one; with --json stdout also
// gets one ligus.error.v1 document:
//
//	{"schema":"ligus.error.v1","tool":"langstroth-relay","command":"check",
//	 "exitCode":2,"error":{"code":"usage","message":"...","fix":"..."}}

// problem is an error with its exit code, a stable machine code and,
// optionally, why it happened and how to fix it.
type problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Why     string `json:"why,omitempty"`
	Fix     string `json:"fix,omitempty"`
	exit    int
}

func (p *problem) Error() string { return p.Message }

// describe is err as a problem for the subcommand cmd ("" for the top level).
func describe(err error, cmd string) problem {
	var p *problem
	if errors.As(err, &p) {
		return *p
	}
	help := "langstroth-relay help"
	if cmd != "" {
		help = "langstroth-relay " + cmd + " --help"
	}
	if errors.Is(err, errUsage) {
		return problem{Code: "usage", Message: strings.TrimPrefix(err.Error(), "usage: "), Fix: "see " + help, exit: exitUsage}
	}
	return problem{Code: "error", Message: err.Error(), exit: exitFailed}
}

// reportError writes err in the shared shape and returns the exit code. A
// failed check already printed its report, so it only sets the code.
func reportError(stderr, stdout io.Writer, err error, cmd string, asJSON bool) int {
	if err == nil {
		return exitOK
	}
	if errors.Is(err, errChecksFailed) {
		return exitFailed
	}
	p := describe(err, cmd)
	fmt.Fprintf(stderr, "langstroth-relay: %s\n", p.Message)
	if p.Why != "" {
		fmt.Fprintf(stderr, "  why: %s\n", p.Why)
	}
	if p.Fix != "" {
		fmt.Fprintf(stderr, "  fix: %s\n", p.Fix)
	}
	if asJSON {
		doc := map[string]any{
			"schema":   "ligus.error.v1",
			"tool":     "langstroth-relay",
			"command":  cmd,
			"exitCode": p.exit,
			"error":    p,
		}
		_ = json.NewEncoder(stdout).Encode(doc)
	}
	return p.exit
}

// wantsJSON reports whether a subcommand's arguments ask for JSON output
// (--json, or -o json / --output json / --output=json).
func wantsJSON(args []string) bool {
	for i, a := range args {
		switch {
		case a == "--":
			return false
		case a == "--json", a == "-o=json", a == "--output=json":
			return true
		case (a == "-o" || a == "--output") && i+1 < len(args) && args[i+1] == "json":
			return true
		}
	}
	return false
}

func splitFlagValues(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if a == "--" {
			return append(out, args[i:]...)
		}
		if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(k, "--") && len(k) > 2 {
			out = append(out, k, v)
			continue
		}
		out = append(out, a)
	}
	return out
}
