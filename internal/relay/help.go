package relay

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// The binary's built-in documentation: `langstroth-relay help [topic]`.
// Everything an operator or a coding agent needs to run the relay is here,
// so nobody needs the docs site to deploy it. settings_test.go checks that
// the config topic lists every variable the code reads.

// Exit codes: one table for every Ligus tool (ligus, the deploy scripts,
// this relay; `ligus help exit-codes`). This binary uses 0, 1 and 2.
const (
	exitOK      = 0
	exitFailed  = 1 // a check failed, or the server could not start
	exitUsage   = 2 // unknown command, flag or topic
	usageFooter = "Run `langstroth-relay help <topic>` for more. Topics: "
)

// exitCodesText is the shared table, in every command's help.
const exitCodesText = `Exit codes (one table for every Ligus tool; this binary uses 0, 1 and 2):
  0  done (warnings allowed)
  1  failed: a check failed or the server could not start (read fix)
  2  usage error: unknown command, flag or topic, or a missing value
  3  a precondition you can fix is missing (a tool, a file, a sign-in)
  4  a person must decide or act; stop and ask
  5  not found
  6  refused by workspace rules (a role, lockdown, agent access)
  7  offline, unreachable, or timed out; retry with backoff
Errors print as "langstroth-relay: <what>" with indented why: and fix: lines;
with --json a {"schema":"ligus.error.v1",...,"error":{"code","message",
"why","fix"}} document also goes to stdout.
`

// errUsage marks a command-line mistake (exit 2, not 1).
var errUsage = errors.New("usage")

func usageErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errUsage}, args...)...)
}

var usageText = `NAME
  langstroth-relay - the Langstroth sync relay for Ligus (Ligus is short for
  ligustica). It stores and forwards end-to-end encrypted envelopes; it never
  holds a workspace key or sees plaintext.

  Endpoints: GET / (status page), GET /v1/health ("ok", or 503 when the store
  is unreachable), GET /metrics (Prometheus; set LANGSTROTH_RELAY_METRICS_TOKEN).

USAGE
  langstroth-relay                    run the server (configured by environment)
  langstroth-relay <command> [flags]
  langstroth-relay help [topic] | help --json
  langstroth-relay <command> --help   help for one command

COMMANDS
  check [--json] [--url URL]          check this environment's settings without
                                      serving; with --url also probe a running relay
  keygen                              print a new Ed25519 token-issuer keypair
  issue --key HEX --sub ID [flags]    mint a signed hrt1 access token
  bootstrap-agent --key HEX --relay URL --room ID --sub ID [flags]
                                      print one command that enrolls a headless agent
  restore [--from NAME|PATH] [--json] with the relay stopped, make a backup the
                                      data dir's relay-state.json (help backup)
  help [topic]                        this text, or a topic below; --json: every
                                      command and topic as one ligus.help.v1 document

EXAMPLES
  langstroth-relay                         # http on 0.0.0.0:8443, memory only
  curl -fsS http://localhost:8443/v1/health # prints ok
  LANGSTROTH_RELAY_DATA_DIR=./data langstroth-relay   # keep state across restarts

ENVIRONMENT
  Configuration is environment variables only, named LANGSTROTH_RELAY_* (plus
  PORT and DATABASE_URL). Any of them can come from a file instead: <NAME>_FILE,
  or $LANGSTROTH_RELAY_SECRETS_DIR/<NAME>. ` + "`langstroth-relay help config`" + ` lists
  every variable with its default.

  The relay refuses to start (exit 1, the reason on stderr) when:
  - a HIVE_RELAY_* or LIGUS_RELAY_* variable is set, or a file with such a name
    is in LANGSTROTH_RELAY_SECRETS_DIR. These names from before the Langstroth
    rename are not read; the error names the LANGSTROTH_RELAY_* replacement,
    never the value. LIGUS_RELAY_URL (a Ligus client setting) is exempt.
  - a <NAME>_FILE or a file in LANGSTROTH_RELAY_SECRETS_DIR can't be read or
    is empty (an unreadable secret would otherwise read as unset: an open
    gate, or the memory store);
  - LANGSTROTH_RELAY_TOKEN_PUBKEY or _TOKEN_PUBKEY_NEXT is not an Ed25519 key,
    _NEXT is set without the first key, or LANGSTROTH_RELAY_TENANTS is empty,
    lacks LANGSTROTH_RELAY_TOKEN_PUBKEY or lacks WORKSPACE_SCOPE=strict;
  - LANGSTROTH_RELAY_TENANCY doesn't parse or conflicts with another setting;
  - the removed LANGSTROTH_RELAY_MAX_ENVELOPES or _RETENTION_DAYS is set to
    anything but 0;
  - LANGSTROTH_RELAY_ROUTES is not all or tunnel, or is tunnel with the
    tunnel off;
  - an APNs key, key id or team id is set while LANGSTROTH_RELAY_TUNNEL is off;
  - LANGSTROTH_RELAY_REQUIRE_DB is on without DATABASE_URL, or Postgres can't
    be reached or migrated;
  - LANGSTROTH_RELAY_MAILBOXES is not on, off or token, or is token on a
    relay with no access gate;
  - a LANGSTROTH_RELAY_BACKUP* setting doesn't parse, or relay-state.json is
    missing or unreadable and there are backups but none passes its checks
    (help backup).
  ` + "`langstroth-relay check`" + ` runs the same checks without serving (it doesn't
  contact Postgres).

EXIT CODES
` + indent(exitCodesText) + `
SEE ALSO
  langstroth-relay help deploy, help troubleshooting, help exit-codes
  ligus doctor and ligus help checks (a Ligus device's checks, which name
  langstroth-relay check when the relay is the problem)
`

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n") + "\n"
}

// helpTopic is one `langstroth-relay help <name>` page.
type helpTopic struct {
	summary string
	write   func(w io.Writer)
}

func textTopic(s string) func(io.Writer) { return func(w io.Writer) { fmt.Fprint(w, s) } }

func commandTopic(name string) func(io.Writer) {
	return func(w io.Writer) { findCommandHelp(name).write(w) }
}

var helpTopics = map[string]helpTopic{
	"config":          {"every environment variable, its default and meaning", writeConfigHelp},
	"deploy":          {"one-shot deploys: Docker Compose, systemd, Helm (OCI chart), a PaaS", textTopic(deployHelp)},
	"membership":      {"roster mode (LANGSTROTH_RELAY_MEMBERSHIP)", textTopic(membershipHelp)},
	"mailboxes":       {"approval mailboxes for code-match approval (LANGSTROTH_RELAY_MAILBOXES)", textTopic(mailboxesHelp)},
	"tunnel":          {"the phone tunnel between the Ligus iPhone app and ligus serve", textTopic(tunnelHelp)},
	"push":            {"push notifications to your own iPhone app build (APNs)", textTopic(pushHelp)},
	"backup":          {"what to back up and how to restore", textTopic(backupHelp)},
	"troubleshooting": {"common failures and their fixes", textTopic(troubleshootingHelp)},
	"check":           {"the check command", commandTopic("check")},
	"exit-codes":      {"the exit codes every Ligus tool shares, and the error shape", textTopic(exitCodesText)},
	"keygen":          {"the keygen command", commandTopic("keygen")},
	"issue":           {"the issue command", commandTopic("issue")},
	"bootstrap-agent": {"the bootstrap-agent command", commandTopic("bootstrap-agent")},
	"restore":         {"the restore command", commandTopic("restore")},
}

func topicNames() []string {
	names := make([]string, 0, len(helpTopics))
	for n := range helpTopics {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// writeUsage prints the top-level help.
func writeUsage(w io.Writer) {
	fmt.Fprint(w, usageText)
	fmt.Fprintln(w, "\nHelp topics (langstroth-relay help <topic>):")
	for _, n := range topicNames() {
		fmt.Fprintf(w, "  %-16s %s\n", n, helpTopics[n].summary)
	}
}

// cmdHelp prints the top-level help or one topic. An unknown topic is a
// usage error.
func cmdHelp(args []string, w io.Writer) error {
	if len(args) == 1 && (args[0] == "--json" || args[0] == "-o=json") || len(args) == 2 && args[0] == "-o" && args[1] == "json" {
		return writeHelpJSON(w)
	}
	if len(args) == 0 {
		writeUsage(w)
		return nil
	}
	if len(args) > 1 {
		return usageErrorf("help takes one topic, got %d", len(args))
	}
	t, ok := helpTopics[strings.TrimPrefix(args[0], "--")]
	if !ok {
		return usageErrorf("no help topic %q. %s%s", args[0], usageFooter, strings.Join(topicNames(), ", "))
	}
	t.write(w)
	return nil
}

// wantsHelp reports whether a subcommand's arguments ask for its help.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			return true
		}
	}
	return false
}

func writeConfigHelp(w io.Writer) {
	fmt.Fprint(w, `Configuration: environment variables

Every setting is an environment variable; set them before the relay starts.

Settings from files (for secrets): a setting X that is unset in the
environment is read from the file named by X_FILE, else from the file
$LANGSTROTH_RELAY_SECRETS_DIR/X. The environment wins over X_FILE, which wins
over the directory; one trailing newline is trimmed. Examples:
DATABASE_URL_FILE, LANGSTROTH_RELAY_METRICS_TOKEN_FILE,
LANGSTROTH_RELAY_TENANCY_FILE. LANGSTROTH_RELAY_APNS_KEY_FILE keeps its own
meaning (the path of the APNs key). An unreadable or empty file stops the
relay at startup.

Old names: HIVE_RELAY_* and LIGUS_RELAY_* are not read, and the relay refuses
to start while one is set (LIGUS_RELAY_URL, a Ligus client setting, is
exempt). Rename each to LANGSTROTH_RELAY_<same suffix>.

Storage: DATABASE_URL set -> Postgres; else LANGSTROTH_RELAY_DATA_DIR set ->
memory plus a JSON snapshot; else memory only (lost on restart).

Rates are tokens per second per client. A client is the TCP peer, or the
address a trusted proxy names (LANGSTROTH_RELAY_TRUSTED_PROXIES); IPv6 is
keyed on its /64. A 429 or 507 answer carries Retry-After and a JSON error
code; a refused write stored nothing.
`)
	writeSettings(w)
	fmt.Fprint(w, `
Not read by the relay itself but useful next to it: GOMEMLIMIT (Go runtime;
for example 800MiB on a 1 GB machine).
`)
}

const deployHelp = `Deploy: one-shot sequences

Every sequence ends with a health check that prints "ok". Replace <version>
with a release, for example 0.4.0 (images and charts are tagged without the
leading v). Three rules hold everywhere:
  - One instance per URL: pairing codes, live push and rate limits live in
    the process. Replace the relay rather than rolling it.
  - HTTPS in front: clients require https. The relay serves plain HTTP; put
    a TLS reverse proxy, an ingress or the platform's edge in front, and set
    LANGSTROTH_RELAY_TRUSTED_PROXIES to the proxy's address so rate limits
    see real clients. Caddy example:
      relay.example.com { reverse_proxy 127.0.0.1:8443 }
  - Health check: GET /v1/health answers 200 "ok" (503 when the store is
    down).
After deploying, give the relay's https URL to whoever creates a Ligus
workspace; it travels inside every invite.

Docker Compose (one Linux host; Compose v2.24 or later)
  Files: deploy/compose/langstroth-relay/ in a checkout. compose.yaml runs the
  relay with Postgres; compose.standalone.yaml runs the relay alone with a
  snapshot on a volume. Both publish 127.0.0.1:8443 for a proxy on the host,
  and "--profile tls" adds Caddy for RELAY_DOMAIN (set in .env).

    git clone https://github.com/langstroth-ai/langstroth-relay
    cd langstroth-relay/deploy/compose/langstroth-relay
    cp .env.example .env && chmod 600 .env
    sed -i "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$(openssl rand -hex 24)/" .env
    docker compose up -d                # or: docker compose --profile tls up -d
    curl -fsS http://127.0.0.1:8443/v1/health

  Standalone: "docker compose -f compose.standalone.yaml up -d". The image is
  distroless (no shell, UID 65532), so check from the host, or run
  "docker compose exec relay /usr/local/bin/langstroth-relay check".

systemd (a Linux host without containers)
  Files: deploy/systemd/langstroth-relay.service (hardened, DynamicUser) and
  deploy/systemd/langstroth-relay.env (the settings, installed as
  /etc/langstroth-relay/env). From a checkout, with Go 1.27 or newer:

    git clone https://github.com/langstroth-ai/langstroth-relay && cd langstroth-relay
    git checkout v<version>
    CGO_ENABLED=0 go build -trimpath -o langstroth-relay ./cmd/langstroth-relay
    sudo install -m 0755 langstroth-relay /usr/local/bin/langstroth-relay
    sudo install -d -m 0750 /etc/langstroth-relay
    sudo install -m 0640 deploy/systemd/langstroth-relay.env /etc/langstroth-relay/env
    sudo install -m 0644 deploy/systemd/langstroth-relay.service /etc/systemd/system/
    sudo systemctl daemon-reload && sudo systemctl enable --now langstroth-relay
    curl -fsS http://127.0.0.1:8443/v1/health

  Without Go, copy the binary out of the image instead of building it:
    docker create --name lr ghcr.io/langstroth-ai/langstroth-relay:<version>
    docker cp lr:/usr/local/bin/langstroth-relay . && docker rm lr
  It listens on 127.0.0.1:8443; put a TLS proxy in front. Without
  DATABASE_URL in the env file, state lives in /var/lib/langstroth-relay.
  Logs: "journalctl -u langstroth-relay". A checkout's deploy/agent/local.sh
  runs the same binary on loopback without systemd.

Helm (Kubernetes, the published OCI chart)
    helm install langstroth-relay \
      oci://ghcr.io/langstroth-ai/charts/langstroth-relay --version <version> \
      -n langstroth-relay --create-namespace --wait
    helm test langstroth-relay -n langstroth-relay
    kubectl -n langstroth-relay port-forward svc/langstroth-relay 8443:8443 &
    curl -fsS http://127.0.0.1:8443/v1/health

  That is one pod on the memory store (state is lost when the pod restarts).
  For a team: --set database.mode=bundled, cnpg or external (Postgres; the
  chart then splits sync and tunnel pods and sets
  LANGSTROTH_RELAY_REQUIRE_DB=1), and ingress.* with TLS. Settings go in
  config.env, secrets in a Secret named by config.existingSecret (mounted
  as LANGSTROTH_RELAY_SECRETS_DIR). Values: "helm show values
  oci://ghcr.io/langstroth-ai/charts/langstroth-relay --version <version>".
  For coding agents, the checkout's deploy/agent/*.sh scripts (see AGENTS.md)
  run preflight, secrets, install and verify non-interactively with --json.

On a PaaS (any platform that runs a container image)
  - Image: ghcr.io/langstroth-ai/langstroth-relay:<version>, pinned. It runs
    as UID 65532, so a mounted volume must be writable by that user.
  - Port: $PORT when the platform sets it, else LANGSTROTH_RELAY_ADDR, else
    0.0.0.0:8443.
  - Settings: environment variables ("langstroth-relay help config"); secrets
    as the platform's secrets, or as files (X_FILE).
  - Storage: the platform's Postgres in DATABASE_URL with
    LANGSTROTH_RELAY_REQUIRE_DB=1, or a volume with LANGSTROTH_RELAY_DATA_DIR
    pointing at it.
  - Health check: HTTP GET /v1/health.
  - Exactly one instance, always on (no scale-to-zero: tunnels and event
    streams are long-lived). TLS is the platform's edge; set
    LANGSTROTH_RELAY_TRUSTED_PROXIES to its proxy range if it documents one.

After any deploy
  langstroth-relay check --url https://relay.example.com   # health and tunnel
  Gate access before sharing a public URL: see "langstroth-relay help issue"
  and LANGSTROTH_RELAY_TOKEN_PUBKEY in "langstroth-relay help config".
`

const membershipHelp = `Membership: roster mode

Deprecated for Ligus workspaces (Ligus 0.4 and later): Ligus keeps its own
signed roster in the workspace log and never reads this one, so a relay
roster on a Ligus workspace can drift from the real one. Gate a Ligus relay
with signed tokens and LANGSTROTH_RELAY_WORKSPACE_SCOPE instead. Roster mode
stays supported for other clients.

Off by default: any client that passes the access gate may read and write
any workspace whose id it knows. Turn on relay-managed membership with

  LANGSTROTH_RELAY_MEMBERSHIP=1        (1 or true; anything else is off)

The relay then keeps a roster of {account, role} per workspace and enforces
it. It still never holds a workspace key or sees plaintext: the roster is
account ids, logins and coarse roles. Roles, highest first: owner, admin,
contributor, viewer.

  - A workspace is unclaimed until an identified caller sends
    POST /v1/workspaces/{id}/members/claim and becomes its owner. An
    unclaimed workspace behaves as if membership were off.
  - On a claimed workspace the caller's token subject (sub) must be on the
    roster: reads and writes (envelopes, presence, candidates, blob uploads)
    need any role, viewer included; key rotations need contributor or above;
    roster changes and invites need admin or above; a blob delete needs its
    uploader or an admin. Nobody can grant a role above their own
    or change a member who outranks them, and the last owner can't be
    removed or demoted.
  - Routes: GET/POST .../members, DELETE .../members/{account},
    POST .../members/claim, POST/GET .../invites,
    DELETE .../invites/{inviteId}, POST .../join (redeem an invite code).

Who the caller is comes from a signed hrt1 token's sub:
  - LANGSTROTH_RELAY_TOKEN_PUBKEY: every request needs a token (gate and
    identity), or
  - LANGSTROTH_RELAY_MEMBERSHIP_PUBKEY: the relay stays open for unclaimed
    workspaces and uses a signed token, when one is sent, as identity.
Mint tokens with "langstroth-relay issue"; "langstroth-relay
bootstrap-agent --admin-sub ..." adds a headless agent to a roster.

Attested mode (LANGSTROTH_RELAY_MEMBERSHIP=attested, where the workspace's
own admin devices publish a signed member list and every request carries a
member proof) is in design and not built. Setting the value today leaves
membership off.
`

const mailboxesHelp = `Mailboxes: approval mailboxes for code-match approval

On by default. A Ligus workspace's members register one approval mailbox per
key epoch. A new box, computer or phone puts a sealed request into it; an
Admin's device and the requester then trade a few sealed messages under that
request, compare a short code on both screens, and the requester deletes the
request once it has its answer. The relay never holds a key that opens any
of it: it checks signatures, hashes and limits only.

  LANGSTROTH_RELAY_MAILBOXES=on      (default) the routes below
  LANGSTROTH_RELAY_MAILBOXES=off     every /v1/mailboxes/* route answers 404;
                                     Ligus falls back to pasting codes
  LANGSTROTH_RELAY_MAILBOXES=token   as on, and a put also needs a valid
                                     access token (needs an access gate:
                                     LANGSTROTH_RELAY_TOKEN_PUBKEY,
                                     _ACCESS_TOKENS or _TENANCY)

Routes (JSON; ids are 32 lowercase hex characters):
  GET    /v1/mailboxes/{mailbox}                         no auth: {mailbox, counter}
  PUT    /v1/mailboxes/{mailbox}                         register or reset, signed
  PUT    /v1/mailboxes/{mailbox}/requests/{rid}          put a request: X-Ligus-Put-Cap
  GET    /v1/mailboxes/{mailbox}/requests                list: a member
  POST   /v1/mailboxes/{mailbox}/requests/{rid}/msgs     a member or the requester
  GET    /v1/mailboxes/{mailbox}/requests/{rid}/msgs?after=N&wait=S
                                                         long poll up to 25 s
  DELETE /v1/mailboxes/{mailbox}/requests/{rid}          the requester

Who may do what:
  - Register and reset are self-certifying: the mailbox id is the first 16
    bytes of SHA-256 of the mailbox's Ed25519 key, and the body is signed by
    that key, so only a holder of the workspace key can claim or reset one.
    The next counter (current + 1) resets the put cap; the old one stops
    working at once. Any other counter is 409 stale_counter.
  - A member signs X-Ligus-Mailbox-Auth with that key (a timestamp within
    300 s of the relay's clock). On a token-gated relay members (and
    register) also send their usual access token.
  - A requester has no token: its X-Ligus-Read-Key opens its own request
    only. A put needs the mailbox's current put cap (X-Ligus-Put-Cap), and
    a token too with LANGSTROTH_RELAY_MAILBOXES=token.
  The put cap is in the workspace's approval address, which is not a secret:
  anyone holding it can put requests until an Admin resets the address.
  That is the only write the relay takes without a signature or a token,
  and the limits below bound it. Every key holder can open a request and so
  learns its read key; at worst that disrupts that one request.
  On a tenancy relay a mailbox belongs to the registering token's tenant.

Limits: 64 live requests per mailbox (429 mailbox_full), 10 messages per
request (429 too_many_messages), a request and its messages expire 15
minutes after the put, 10 puts a minute per client address (160 per IPv6
/48, one a second per mailbox after a burst of 64; 429 mailbox_put_rate), a
request blob of at most 8 KiB and a message blob of at most 32 KiB (413),
and the same blob twice on one rid is 409 duplicate_message. A rid is never
overwritten, and a deleted or expired rid can't be put again for 15 minutes
past its expiry (409 rid_exists).
Registrations: LANGSTROTH_RELAY_MAX_MAILBOXES per process,
LANGSTROTH_RELAY_MAX_MAILBOXES_PER_CLIENT per client address,
LANGSTROTH_RELAY_MAX_MAILBOXES_PER_TENANT per tenant. Blob bytes: 4 MiB per
mailbox, LANGSTROTH_RELAY_MAILBOX_MAX_BYTES_PER_CLIENT for the mailboxes one
client address (IPv4 address or IPv6 /48) registered, and
LANGSTROTH_RELAY_MAILBOX_MAX_BYTES in all (507 mailbox_storage; help
config). A mailbox with no live request and no member call for 24 hours is
forgotten. Long polls: 16 per client and 64 per IPv6 /48 (429
mailbox_polls); near a mailbox's or the relay's cap they answer at once.

Storage: in the relay process only (like pairing codes), never in the
snapshot or Postgres. A restart forgets every mailbox and request; members
register again when a call answers mailbox_unknown. The Helm chart runs one
sync process; the tunnel-only pod answers 404 here.

Errors are JSON problem documents: {"error":"<code>","code":"<code>",
"message":...}, plus retryAfter and Retry-After on a 429 or 507. Codes:
mailbox_unknown, request_unknown, stale_counter (409, with the current
counter), rid_exists, mailbox_full, too_many_messages, duplicate_message, mailbox_put_rate,
mailbox_register, mailboxes_full, mailbox_storage, mailbox_polls,
bad_put_cap, bad_read_key, bad_member_auth, auth_required, token_required,
bad_signature, mailbox_mismatch, invalid_request, too_large.

Metrics: hive_relay_mailboxes, hive_relay_mailbox_requests,
hive_relay_mailbox_bytes and hive_relay_mailbox_refusals_total{code}.

Short link codes (Ligus 0.5), on and off with the mailboxes: a person's
device posts a short code for its mailbox, and their new device types it
instead of pasting the approval address. The client stretches the 6-symbol
code with Argon2id into a lookup id and a key that seals the address, so the
relay sees neither the code nor the address.
  PUT    /v1/mailboxes/{mailbox}/codes/{code}   a member: post or re-post
                                                {blob, ttlSecs 60..600}
  GET    /v1/mailboxes/{mailbox}/codes/{code}   a member: {code, expiresAt, used}
  DELETE /v1/mailboxes/{mailbox}/codes/{code}   a member
  POST   /v1/link-codes/{code}                  anyone: {mailbox, blob}; single-use
Limits: 10 minutes at most, 4 live codes per mailbox (429 link_codes_full),
10 posts a minute per mailbox (429 link_code_rate), 32 posted from one
IPv4 address or IPv6 /48 and 10000 per process (429 and 507
link_codes_full), a 2 KiB blob, and 10 resolves a minute per client address
(160 per IPv6 /48; 429 link_code_attempts), found or not. Codes are in
memory too: after a restart the status call answers code_unknown and the
device posts the same code again. Codes: code_unknown, code_taken,
code_used, link_codes_full, link_code_rate, link_code_attempts. Metric:
hive_relay_link_codes. docs/reference.md, "Short link codes".
`

const tunnelHelp = `Tunnel: the Ligus iPhone app to a ligus serve host

The tunnel lets the phone app reach a "ligus serve" host through the relay,
with no VPN and no inbound port on the host. Phone and host run a Noise
session end to end; the relay checks who may connect, then forwards bytes it
can't read.

Turn it on (off by default; /v1/tunnel/* answers 404 when off):
  LANGSTROTH_RELAY_TUNNEL=open                      any host may register
  LANGSTROTH_RELAY_TUNNEL=allowlist
  LANGSTROTH_RELAY_TUNNEL_HOSTS=<host_id>,<host_id>  only these hosts

With an access gate configured (LANGSTROTH_RELAY_TOKEN_PUBKEY or
LANGSTROTH_RELAY_ACCESS_TOKENS), a host's first registration must present a
token that passes it ("ligus remote enable --reg-token ...").

Routes (WebSockets need the subprotocol hive-tunnel.1):
  GET  /v1/tunnel/info      {"versions":[1],"push":bool,"billing":"none"}
  GET  /v1/tunnel/host      host control socket
  GET  /v1/tunnel/accept    host data socket per phone session
  GET  /v1/tunnel/connect   phone
  POST /v1/tunnel/revoke    host revokes one phone

Run one process for the tunnel: hosts' control connections live in memory.
The Helm chart's split topology runs a separate tunnel pod
(LANGSTROTH_RELAY_ROUTES=tunnel). Behind a proxy, allow WebSocket upgrades
and long idle timeouts (sockets are pinged every 20 s). Push limits per
host: LANGSTROTH_RELAY_TUNNEL_PUSH_RATE, _BURST, _PER_DAY. Push itself:
"langstroth-relay help push". Check it: "langstroth-relay check --url URL"
reports the tunnel and push state.
`

const pushHelp = `Push: notifications from your own relay (APNs)

Push goes out only through the tunnel, and APNs accepts a push only when it
is signed with a key from the Apple developer team that publishes the app. So
your relay can push to iPhone app builds you sign under your own Apple
developer account, not to someone else's build. Without push, "ligus serve"
falls back to ntfy when it is configured.

1. In the Apple developer portal (Certificates, Identifiers & Profiles,
   Keys) create a key with Apple Push Notifications service enabled.
   Download the .p8 once; note its Key ID and your Team ID.
2. Run the relay with the tunnel on and the key:

     LANGSTROTH_RELAY_TUNNEL=open \
     LANGSTROTH_RELAY_APNS_KEY_FILE=/secrets/AuthKey_ABC123DEFG.p8 \
     LANGSTROTH_RELAY_APNS_KEY_ID=ABC123DEFG \
     LANGSTROTH_RELAY_APNS_TEAM_ID=<team id> \
     LANGSTROTH_RELAY_APNS_TOPIC=<your app's bundle id> \
     langstroth-relay

   LANGSTROTH_RELAY_APNS_KEY holds the PEM inline instead of a file.
   LANGSTROTH_RELAY_APNS_TOPICS lists more bundle ids (a dev build next to
   the App Store build). LANGSTROTH_RELAY_APNS_ENV: auto (default; follows
   what each phone reports), sandbox or production.
3. Check: the log says "tunnel push via APNs", and GET /v1/tunnel/info
   reports "push": true ("langstroth-relay check --url URL" shows it).

An incomplete APNs configuration or a key that doesn't parse is logged and
leaves push off; the relay still starts. APNs settings on a process whose
tunnel is off stop it from starting (keep the key on the tunnel pod only).
Every push is logged as "tunnel push" with a result (sent, no_push_token,
push_failed, rate_limited, revoked, push_unavailable), never the device
token or the payload.
`

const backupHelp = `Backup and restore

The relay holds ciphertext, key rotations (encrypted), rosters, pairing
state, tunnel host registrations and push tokens. Devices keep full copies
of their workspaces and push again to an empty relay, but rosters, tunnel
registrations and push tokens live only here.

Memory store with LANGSTROTH_RELAY_DATA_DIR: built-in backups (on by default)
  The state is one file, $LANGSTROTH_RELAY_DATA_DIR/relay-state.json,
  rewritten atomically (temp file plus rename) every 5 s and on shutdown.
  Every hour (LANGSTROTH_RELAY_BACKUP_INTERVAL) and on shutdown the relay
  copies it, gzipped, to
    $LANGSTROTH_RELAY_DATA_DIR/backups/relay-backup-<UTC time>.json.gz
  (LANGSTROTH_RELAY_BACKUP_DIR to put them elsewhere; directory mode 0700,
  files 0600) and keeps the newest 24 (LANGSTROTH_RELAY_BACKUP_KEEP). A state
  that is empty, or the same as the newest backup, is not backed up again, so
  a wiped or idle relay never rotates good backups away. When the newest
  backup is older than the interval at startup, the first one comes a minute
  after start. Backups are ordered by the UTC time in their name; one named
  more than 10 minutes in the future (a wrong clock) is never restored or
  rotated, and check warns about it.

  At startup, when relay-state.json is missing or unreadable, the relay
  restores the newest backup that passes its checks and logs "restored state
  from backup". An unreadable relay-state.json is first moved aside to
  relay-state.json.unreadable-<time>, never deleted. A relay-state.json that
  reads is never replaced. When there are backups but none passes its
  checks, the relay refuses to start (starting empty would leave every
  roster unclaimed); fix the files, restore one by hand, or set
  LANGSTROTH_RELAY_BACKUP_RESTORE=off to start empty. With no backups at all
  it starts empty. LANGSTROTH_RELAY_BACKUP=off turns backups off.

  A restore goes back in time: everything since that backup is undone,
  including removed members, revoked tunnel hosts and phones, deleted
  tokens and used invites. After a restore, check rosters and repeat any
  removal or revocation made since the backup's time (the log names it).

  Restore an older backup by hand:
    1. Stop the relay. (restore refuses while relay-state.json changed in
       the last 15 s; it can't tell a relay whose flushes are failing, so
       make sure the process is gone.)
    2. ls $LANGSTROTH_RELAY_DATA_DIR/backups
    3. LANGSTROTH_RELAY_DATA_DIR=/data langstroth-relay restore --from relay-backup-20261124T030000Z.json.gz
       (no --from: the newest). The current relay-state.json moves to
       relay-state.json.before-restore-<time>.
    4. Start the relay.
  A backup is a gzipped relay-state.json, so gunzip -c also works.

  To start over on purpose, stop the relay and remove relay-state.json and
  the backup directory (or set LANGSTROTH_RELAY_BACKUP_RESTORE=off).

  What a restore checks: the file is a regular file (not a symlink) with an
  exact backup name, owned by the relay's user and not writable by group or
  others, in a directory others can't write; it decompresses to at most 4 GiB
  (4 x LANGSTROTH_RELAY_MAX_STORE_BYTES + 256 MiB when that is set); and it
  parses as a relay snapshot. A backup is as trusted as relay-state.json:
  whoever can write either controls the relay's rosters, so keep both on
  storage only the relay's user can write.

  Limits:
  - Backups sit on the same volume as the data unless
    LANGSTROTH_RELAY_BACKUP_DIR points elsewhere: they survive a damaged or
    deleted relay-state.json, not the loss of the volume. Copy the backup
    directory off the host (restic, rclone, a volume snapshot) for that.
  - Attachment bytes (blobs/) are not in a backup. The snapshot lists each
    blob with its SHA-256; at load any whose bytes are missing or differ are
    dropped. Copy blobs/ alongside if attachments matter.
  - A backup is at most one flush (5 s) behind the live state, plus the
    interval: changes after the last backup are lost on a restore.
  - Disk: up to LANGSTROTH_RELAY_BACKUP_KEEP compressed copies of the state.
  - Backups hold what relay-state.json holds (token and invite-code hashes,
    push tokens, rosters; never a workspace key or plaintext). Treat them
    as secret.

Postgres (DATABASE_URL): not covered by the built-in backups
  Back up the database, not the pods: a managed database's point-in-time
  recovery, CloudNativePG with WAL archiving (Helm database.mode=cnpg), or
    pg_dump --format=custom --file=relay.dump "$DATABASE_URL"
  Restore: stop the relay, pg_restore --clean --no-owner --dbname=... the
  dump, start the relay. The Helm chart's bundled Postgres takes nightly
  dumps and restores with database.bundled.backups.restore.file (see the
  chart README). Migrations at startup are additive, so you can roll the
  relay back one release.

Memory only (no DATABASE_URL, no LANGSTROTH_RELAY_DATA_DIR): nothing to back
up; everything is lost on restart.

Keep backups apart from the APNs key: together they are a push channel to
every registered phone. The token issuer's private key (from keygen) is not
on the relay; back it up wherever you issue tokens.

Check: langstroth-relay check (the "backup" line: how many backups, the
newest, the settings; fail when the directory is unsafe).
`

const troubleshootingHelp = `Troubleshooting

Start here:
  langstroth-relay check            # the startup checks, without serving
  langstroth-relay check --json --url https://relay.example.com

The relay exits at startup with "... names are no longer read: rename ..."
  A HIVE_RELAY_* or LIGUS_RELAY_* variable (or such a file in
  LANGSTROTH_RELAY_SECRETS_DIR) is set. Rename it as the message says: set
  the new name wherever the relay's settings live (env file, Secret or the
  platform's secrets), then unset the old one.

"cannot read" or "is empty" for a _FILE or LANGSTROTH_RELAY_SECRETS_DIR
  The file is missing, unreadable by the relay's user (UID 65532 in the
  published image) or empty. Fix the path, mode or fsGroup.

"LANGSTROTH_RELAY_REQUIRE_DB is set but DATABASE_URL is empty"
  Set DATABASE_URL (or DATABASE_URL_FILE), or unset REQUIRE_DB to allow the
  memory store.

"... is removed" (MAX_ENVELOPES, RETENTION_DAYS)
  Unset it or set it to 0; use LANGSTROTH_RELAY_ROOM_MAX_ENVELOPES or
  LANGSTROTH_RELAY_ROOM_RETENTION_DAYS.

Every client gets 401
  The access gate is on (LANGSTROTH_RELAY_TOKEN_PUBKEY or _ACCESS_TOKENS)
  and clients send no token or a token from another issuer or expired.
  Issue tokens with "langstroth-relay issue" against the matching key.

403 on one workspace
  WORKSPACE_SCOPE confines the token to other workspaces, or membership is
  on and the token's sub is not on the claimed workspace's roster (or its
  role is too low). See "langstroth-relay help membership".

429 for everyone at once
  The relay sits behind a proxy without LANGSTROTH_RELAY_TRUSTED_PROXIES, so
  every client shares the proxy's address and one rate-limit bucket. Set the
  proxy's CIDR.

507 refusals
  A quota is full (the JSON error says which: room_bytes, total_rooms,
  blob_room_bytes, ...). Raise the limit or let retention free space; the
  relay never evicts envelopes.

GET /v1/health answers 503
  The store is unreachable (Postgres down, wrong URL or credentials).

"no backup could be restored" at startup
  relay-state.json is missing or unreadable and every backup failed a check
  (the message says which: owner, mode, size, not a snapshot, named in the
  future). Fix the files (owned by the relay's user, chmod 600, the
  directory not writable by others), or restore one by hand with
  "langstroth-relay restore --from PATH", or set
  LANGSTROTH_RELAY_BACKUP_RESTORE=off to start empty.

State is gone after a restart
  Neither DATABASE_URL nor LANGSTROTH_RELAY_DATA_DIR is set (memory only), or
  the data directory isn't on a persistent volume.

The phone can't connect through the tunnel
  /v1/tunnel/info answers 404: LANGSTROTH_RELAY_TUNNEL is off. With
  allowlist, the host's host_id must be in LANGSTROTH_RELAY_TUNNEL_HOSTS.
  A proxy must pass WebSocket upgrades and keep idle sockets for 60 s or
  more. More than one relay process: route the tunnel to one of them.

No push notifications
  /v1/tunnel/info shows "push": false: the APNs settings are incomplete or
  the key doesn't parse (the startup log says which). See
  "langstroth-relay help push".

Clients refuse the URL
  Clients require https. Terminate TLS in front of the relay.
`
