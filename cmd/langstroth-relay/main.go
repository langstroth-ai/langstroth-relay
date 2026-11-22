// Command langstroth-relay is the open-source Langstroth relay binary: a content-blind
// rendezvous + envelope-forwarding server. It wires the default (open) behavior
// of the relay package (internal/relay); seams for custom behavior are in
// internal/relay/seams.go.
//
// Run the server (default):   langstroth-relay
// Generate an issuer keypair: langstroth-relay keygen
// Mint an access token:       langstroth-relay issue --key <priv-hex> --sub <id> [...]
// Check a configuration:      langstroth-relay check [--json] [--url URL]
// Everything else:            langstroth-relay help [topic]
package main

import (
	"log/slog"
	"os"

	"github.com/langstroth-ai/langstroth-relay/internal/relay"
)

func main() {
	// Structured JSON logs (request lines come from the observability middleware).
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "help", "--help", "-h":
			exitWith("help", args[1:], relay.Help(args[1:], os.Stdout))
		}
		cmd, ok := commands[args[0]]
		if !ok {
			exitWith("", nil, relay.UsageErrorf("unknown command %q (the server takes no arguments)", args[0]))
		}
		if relay.WantsHelp(args[1:]) {
			exitWith(args[0], args[1:], relay.Help([]string{args[0]}, os.Stdout))
		}
		exitWith(args[0], args[1:], cmd(args[1:]))
	}
	relay.Serve()
}

// commands are the subcommands; no argument runs the server.
var commands = map[string]func(args []string) error{
	"keygen": func(args []string) error {
		if len(args) > 0 {
			return relay.UsageErrorf("keygen takes no arguments")
		}
		return cmdKeygen()
	},
	"issue":           cmdIssue,
	"bootstrap-agent": cmdBootstrapAgent,
	"check":           func(args []string) error { return relay.Check(args, os.Stdout) },
}

// exitWith ends a subcommand: 0 on success, 2 for a usage error, else 1
// (`langstroth-relay help` lists the exit codes). Errors print as
// "langstroth-relay: <what>" with indented why: and fix: lines; with --json
// a ligus.error.v1 document goes to stdout too.
func exitWith(cmd string, args []string, err error) {
	os.Exit(relay.ReportError(os.Stderr, os.Stdout, err, cmd, relay.WantsJSON(args)))
}
