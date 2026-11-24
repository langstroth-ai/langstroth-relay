package relay

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Every command's help has the same sections, in the order every Ligus tool
// uses: NAME, USAGE, FLAGS, EXAMPLES, ENVIRONMENT, EXIT CODES, SEE ALSO.
// `langstroth-relay help --json` prints this registry and the topics.

type flagHelp struct {
	Flag  string `json:"flag"`
	About string `json:"about"`
}

type commandHelp struct {
	Name        string     `json:"name"`
	Summary     string     `json:"summary"`
	Usage       []string   `json:"usage"`
	Description string     `json:"description"`
	Flags       []flagHelp `json:"flags"`
	Examples    []string   `json:"examples"`
	Env         []string   `json:"environment"`
	Exit        []string   `json:"exitCodes"`
	SeeAlso     []string   `json:"seeAlso"`
}

var helpFlag = flagHelp{"-h, --help", "print this help"}

var commandHelps = []commandHelp{
	{
		Name:    "check",
		Summary: "check this environment's settings without serving; with --url also probe a running relay",
		Usage:   []string{"langstroth-relay check [--json | -o json|text] [--url URL]"},
		Description: `Runs the startup checks against this process's environment without serving:
old HIVE_RELAY_*/LIGUS_RELAY_* names, secret files, the access gate,
tenancy, removed settings, routes and the tunnel, push, storage, backups,
the metrics token and the listen address. It does not connect to Postgres.

With --url, also probes a running relay: GET URL/v1/health must answer 200,
and GET URL/v1/tunnel/info reports whether the tunnel and push are on.

The text form: a header, one line per check (status, title, what was found),
an indented "fix:" line under anything not ok, and a summary such as
"2 warnings · next: langstroth-relay keygen".

--json prints one ligus.checks.v1 report, on one line, on stdout (the shape
ligus doctor --json and the Ligus deploy scripts print too):
  {"schema":"ligus.checks.v1","tool":"langstroth-relay","command":"check",
   "ok":bool,"exitCode":0|1,
   "checks":[{"id","name","title","status","detail","fix"}, ...],
   "next":["langstroth-relay help troubleshooting"]}
status is ok, warn, fail or skip; ok is false when any check failed. id is
the stable key; name holds the same value for one release (0.3.2 printed
name and pass). fix is always present: what to do, or "" when there is
nothing to do (a failed check always has one). next lists commands to run
next. A usage error prints a ligus.error.v1 document instead.`,
		Flags: []flagHelp{
			{"--json", "print one ligus.checks.v1 report on stdout"},
			{"-o, --output FORMAT", "json (the same as --json) or text (the default)"},
			{"--url URL", "base URL of a running relay, for example https://relay.example.com"},
			helpFlag,
		},
		Examples: []string{
			"langstroth-relay check",
			"LANGSTROTH_RELAY_DATA_DIR=/data langstroth-relay check --json",
			"langstroth-relay check --url https://relay.example.com",
		},
		Env: []string{"every LANGSTROTH_RELAY_* setting the server reads (langstroth-relay help config)"},
		Exit: []string{
			"0  no check failed (warnings allowed); ok is true",
			"1  a check failed; ok is false and that check's fix says what to do",
			"2  usage error: an unknown flag, --url without a value",
		},
		SeeAlso: []string{"langstroth-relay help troubleshooting", "langstroth-relay help config",
			"ligus doctor (on a Ligus device: its relay check names this command)", "ligus help checks"},
	},
	{
		Name:    "keygen",
		Summary: "print a new Ed25519 token-issuer keypair",
		Usage:   []string{"langstroth-relay keygen"},
		Description: `Prints a fresh Ed25519 token-issuer keypair as hex:
  private_key=...   keep it where you issue tokens, never on the relay
  public_key=...    set it as LANGSTROTH_RELAY_TOKEN_PUBKEY on the relay

The relay only verifies tokens. With the public key set, every /v1 request
needs a token minted with "langstroth-relay issue --key <private_key>".
During an issuer key change, put the new public key in
LANGSTROTH_RELAY_TOKEN_PUBKEY_NEXT until every client has a new token.`,
		Flags:    []flagHelp{helpFlag},
		Examples: []string{"langstroth-relay keygen > issuer.txt && chmod 600 issuer.txt"},
		Env:      []string{"(none)"},
		Exit:     []string{"0  printed", "2  usage error: keygen takes no arguments"},
		SeeAlso:  []string{"langstroth-relay issue --help", "langstroth-relay help config (Access)"},
	},
	{
		Name:    "issue",
		Summary: "mint a signed hrt1 access token",
		Usage:   []string{"langstroth-relay issue --key HEX --sub ID [flags]"},
		Description: `Mints a signed hrt1 access token and prints it on stdout. Run it where the
issuer private key lives, not on the relay.`,
		Flags: []flagHelp{
			{"--key HEX", "issuer private key (64 hex chars, from keygen); required"},
			{"--sub ID", "subject: the account or device id; required. Membership rosters key on it."},
			{"--exp-days N", "expire after N days (default: never)"},
			{"--workspace ID", "confine the token to this workspace id (repeat; * = any). Enforced with LANGSTROTH_RELAY_WORKSPACE_SCOPE=claims or strict."},
			{"--tenant ID", "tenant id (tid claim), checked against LANGSTROTH_RELAY_TENANTS or the tenancy config"},
			{"--plan NAME", "plan claim (free text, for example team)"},
			{"--max-members N", "max-members claim"},
			{"--retention-days N", "retention-days claim"},
			{"--turn", "turn claim"},
			{"--cap NAME", "capability claim (repeat), for example remove_member"},
			helpFlag,
		},
		Examples: []string{
			`langstroth-relay issue --key "$ISSUER_KEY" --sub alice --exp-days 365`,
			`langstroth-relay issue --key "$ISSUER_KEY" --sub ci-bot --workspace 0f3c... --exp-days 30`,
		},
		Env:     []string{"(none)"},
		Exit:    []string{"0  printed", "2  usage error: an unknown flag, no --sub, a --key that isn't a 64-char hex seed"},
		SeeAlso: []string{"langstroth-relay keygen --help", "langstroth-relay help membership", "ligus help env (LIGUS_RELAY_ACCESS_TOKEN, where a Ligus device takes the token)"},
	},
	{
		Name:    "bootstrap-agent",
		Summary: "print one command that enrolls a headless agent",
		Usage:   []string{"langstroth-relay bootstrap-agent --key HEX --relay URL --room ID --sub ID [flags]"},
		Description: `Prints one command to run on a remote box so a headless Ligus agent joins a
workspace: it installs ligus, enrolls with a minted token and a connection
invite, and starts "ligus worker". The issuer key stays here; only the
agent's token leaves. The command goes to stdout, notes to stderr.`,
		Flags: []flagHelp{
			{"--key HEX", "issuer private key (from keygen); required"},
			{"--relay URL", "the relay's base URL; required"},
			{"--room ID", "the workspace (room) id; required"},
			{"--sub ID", "the agent's subject, for example github:<agent-id>; required"},
			{"--ws-key PASS", "workspace passphrase to put in the invite"},
			{"--admin-sub ID", "an owner or admin of the workspace: with it the agent is added to the roster (membership on) using a one-hour token for that admin"},
			{"--role ROLE", "roster role (default contributor)"},
			{"--label NAME", "worker label (default: the sub with : and / as -)"},
			{"--exp-days N", "agent token lifetime in days (default 365)"},
			helpFlag,
		},
		Examples: []string{`langstroth-relay bootstrap-agent --key "$ISSUER_KEY" --relay https://relay.example.com --room 0f3c... --sub github:my-agent --admin-sub github:me`},
		Env:      []string{"(none)"},
		Exit: []string{
			"0  printed (a failed roster enrollment is a note on stderr)",
			"2  usage error: an unknown flag, a missing --key, --relay, --room or --sub",
		},
		SeeAlso: []string{"ligus help worker", "ligus help box", "langstroth-relay help membership"},
	},
	{
		Name:    "restore",
		Summary: "with the relay stopped, make a backup the data dir's relay-state.json",
		Usage:   []string{"langstroth-relay restore [--from NAME|PATH] [--json | -o json|text]"},
		Description: `Restores the memory store (LANGSTROTH_RELAY_DATA_DIR) from one of its
built-in backups. Stop the relay first: restore refuses while
relay-state.json changed in the last 15 s (a running relay rewrites it every
5 s and would overwrite the restore on shutdown).

Without --from it takes the newest backup in LANGSTROTH_RELAY_BACKUP_DIR
(default <data dir>/backups). --from takes a backup's file name (looked up
in that directory; only relay-backup-YYYYMMDDTHHMMSSZ.json.gz names) or a
path to a backup copied in from elsewhere.

Before anything changes, the backup must be a regular file (not a symlink)
owned by this user and not writable by group or others, decompress within
the size bound and parse as a relay snapshot. The current relay-state.json
is moved aside to relay-state.json.before-restore-<time>, never deleted;
the new one is written mode 0600.

--json prints one ligus.checks.v1 report (command "restore") on success;
a failure prints the error shape (with --json a ligus.error.v1 document).
Postgres relays use the database's own tooling (help backup).`,
		Flags: []flagHelp{
			{"--from NAME|PATH", "a backup file name in the backup directory, or a path (default: the newest backup)"},
			{"--json", "print one ligus.checks.v1 report on stdout"},
			{"-o, --output FORMAT", "json (the same as --json) or text (the default)"},
			helpFlag,
		},
		Examples: []string{
			"LANGSTROTH_RELAY_DATA_DIR=/data langstroth-relay restore",
			"LANGSTROTH_RELAY_DATA_DIR=/data langstroth-relay restore --from relay-backup-20261124T030000Z.json.gz --json",
		},
		Env: []string{
			"LANGSTROTH_RELAY_DATA_DIR   the data directory to restore into (required)",
			"LANGSTROTH_RELAY_BACKUP_DIR where the backups are (default <data dir>/backups)",
			"LANGSTROTH_RELAY_MAX_STORE_BYTES  bounds a backup's decompressed size (4x plus 256 MiB; default 4 GiB)",
			"DATABASE_URL                set: refused (Postgres is not covered)",
		},
		Exit: []string{
			"0  restored",
			"1  refused: the relay looks like it is running, no backup, the backup failed a check, DATABASE_URL is set, a setting doesn't parse",
			"2  usage error: an unknown flag, a --from that is neither a backup name nor a path, LANGSTROTH_RELAY_DATA_DIR unset",
		},
		SeeAlso: []string{"langstroth-relay help backup", "langstroth-relay help config (Backups)", "langstroth-relay check"},
	},
}

func findCommandHelp(name string) *commandHelp {
	for i := range commandHelps {
		if commandHelps[i].Name == name {
			return &commandHelps[i]
		}
	}
	return nil
}

// write prints one command's help in the shared section order.
func (c *commandHelp) write(w io.Writer) {
	fmt.Fprintf(w, "NAME\n  langstroth-relay %s - %s\n\n", c.Name, c.Summary)
	for _, l := range strings.Split(c.Description, "\n") {
		if l == "" {
			fmt.Fprintln(w)
		} else {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
	fmt.Fprint(w, "\nUSAGE\n")
	for _, u := range c.Usage {
		fmt.Fprintf(w, "  %s\n", u)
	}
	fmt.Fprint(w, "\nFLAGS\n")
	for _, f := range c.Flags {
		fmt.Fprintf(w, "  %-20s %s\n", f.Flag, f.About)
	}
	fmt.Fprint(w, "\nEXAMPLES\n")
	for _, x := range c.Examples {
		fmt.Fprintf(w, "  %s\n", x)
	}
	fmt.Fprint(w, "\nENVIRONMENT\n")
	for _, e := range c.Env {
		fmt.Fprintf(w, "  %s\n", e)
	}
	fmt.Fprint(w, "\nEXIT CODES\n")
	for _, e := range c.Exit {
		fmt.Fprintf(w, "  %s\n", e)
	}
	fmt.Fprint(w, "  The full table, shared by every Ligus tool: langstroth-relay help exit-codes.\n")
	fmt.Fprint(w, "\nSEE ALSO\n")
	for _, s := range c.SeeAlso {
		fmt.Fprintf(w, "  %s\n", s)
	}
}

// writeHelpJSON prints the commands and the topics as one document.
func writeHelpJSON(w io.Writer) error {
	type topic struct {
		Name    string `json:"name"`
		Summary string `json:"summary"`
	}
	var topics []topic
	for _, n := range topicNames() {
		if findCommandHelp(n) == nil {
			topics = append(topics, topic{n, helpTopics[n].summary})
		}
	}
	return json.NewEncoder(w).Encode(map[string]any{
		"schema":   "ligus.help.v1",
		"tool":     "langstroth-relay",
		"commands": commandHelps,
		"topics":   topics,
	})
}
