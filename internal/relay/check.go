package relay

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// `langstroth-relay check`: the startup checks Main runs, plus a few
// warnings, without serving. With --url it also probes a running relay.
//
// --json prints one ligus.checks.v1 report on one line, the shape every
// Ligus check command prints (ligus doctor --json; the ligus deploy
// scripts' ligus.agent-deploy.v1 is a profile of it):
// {"schema","tool","command","ok","exitCode","checks","next"}, each check
// {"id","title","status","detail","fix"} with status ok, warn, fail or skip.
// For one release each check also carries "name" (the same value as "id"),
// the key 0.3.2 printed. The deploy/agent scripts' reports carry the same
// keys they always did plus "id"; TestCheckJSONMatchesDeployAgent keeps
// check a superset of them.

// checksSchema is the schema string of check's JSON report.
const checksSchema = "ligus.checks.v1"

type checkResult struct {
	ID     string `json:"id"`
	Name   string `json:"name"` // the same as ID; kept for one release
	Title  string `json:"title"`
	Status string `json:"status"` // ok | warn | fail | skip
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

type checkReport struct {
	Schema   string        `json:"schema"`
	Tool     string        `json:"tool"`
	Command  string        `json:"command"`
	OK       bool          `json:"ok"`
	ExitCode int           `json:"exitCode"`
	Checks   []checkResult `json:"checks"`
	Next     []string      `json:"next"`
}

// checkTitles are the text form's labels.
var checkTitles = map[string]string{
	"old-names":    "old names",
	"secret-files": "secret files",
	"access":       "access gate",
	"tenancy":      "tenancy",
	"removed":      "removed settings",
	"routes":       "routes",
	"mailboxes":    "mailboxes",
	"push":         "push",
	"store":        "store",
	"backup":       "backup",
	"metrics":      "metrics",
	"listen":       "listen",
	"health":       "health",
	"tunnel-info":  "tunnel",
}

// checkHTTPClient is the client --url probes use (replaced in tests).
var checkHTTPClient = &http.Client{Timeout: 10 * time.Second}

func cmdCheck(args []string, w io.Writer) error {
	var asJSON bool
	var url string
	args = splitFlagValues(args)
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--json":
			asJSON = true
		case a == "-o" || a == "--output":
			if i+1 >= len(args) {
				return usageErrorf("%s needs json or text", a)
			}
			i++
			switch args[i] {
			case "json":
				asJSON = true
			case "text":
				asJSON = false
			default:
				return usageErrorf("%s must be json or text, not %q", a, args[i])
			}
		case a == "--url":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return usageErrorf("--url needs a value")
			}
			i++
			url = args[i]
		default:
			return usageErrorf("check: unknown flag %s", a)
		}
	}
	rep := runChecks(os.Environ(), url)
	if asJSON {
		if err := json.NewEncoder(w).Encode(rep); err != nil {
			return err
		}
	} else {
		writeCheckText(w, rep, url)
	}
	if !rep.OK {
		return errChecksFailed
	}
	return nil
}

// writeCheckText prints the text form: a header, one line per check, an
// indented fix line under anything not ok, then a summary.
func writeCheckText(w io.Writer, rep checkReport, url string) {
	if url != "" {
		fmt.Fprintf(w, "langstroth-relay check: this environment's settings, and the relay at %s\n", url)
	} else {
		fmt.Fprintln(w, "langstroth-relay check: this environment's settings")
	}
	counts := map[string]int{}
	for _, c := range rep.Checks {
		counts[c.Status]++
		fmt.Fprintf(w, "%-5s %-17s %s\n", c.Status, c.Title, c.Detail)
		if c.Fix != "" && c.Status != "ok" {
			fmt.Fprintf(w, "      fix: %s\n", c.Fix)
		}
	}
	plural := func(n int, one, many string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, one)
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	var head string
	switch f, wn := counts["fail"], counts["warn"]; {
	case f == 0 && wn == 0:
		head = "all checks passed"
	case f == 0:
		head = plural(wn, "warning", "warnings")
	case wn == 0:
		head = plural(f, "failure", "failures")
	default:
		head = plural(f, "failure", "failures") + ", " + plural(wn, "warning", "warnings")
	}
	next := "nothing to do"
	if len(rep.Next) > 0 {
		next = strings.Join(rep.Next, " ; ")
	}
	fmt.Fprintf(w, "%s · next: %s\n", head, next)
}

var errChecksFailed = fmt.Errorf("one or more checks failed")

func runChecks(environ []string, url string) checkReport {
	rep := checkReport{Schema: checksSchema, Tool: "langstroth-relay", Command: "check", OK: true, Next: []string{}}
	add := func(name, status, detail, fix string) {
		if status == "fail" {
			rep.OK = false
			rep.ExitCode = exitFailed
		}
		title := checkTitles[name]
		if title == "" {
			title = name
		}
		rep.Checks = append(rep.Checks, checkResult{ID: name, Name: name, Title: title, Status: status, Detail: detail, Fix: fix})
	}
	fromErr := func(name string, err error, okDetail, fix string) {
		if err != nil {
			add(name, "fail", err.Error(), fix)
		} else {
			add(name, "ok", okDetail, "")
		}
	}

	fromErr("old-names", removedEnvError(environ), "no HIVE_RELAY_* or LIGUS_RELAY_* variables",
		"rename each variable as the message says, then unset the old name")
	fromErr("secret-files", secretFilesConfigError(os.Getenv), "every _FILE and secrets-dir file is readable",
		"fix the file's path, owner or mode, or unset the _FILE variable")

	// Access gate.
	if err := entitlementConfigError(); err != nil {
		add("access", "fail", err.Error(), "see `langstroth-relay help config` (Access)")
	} else {
		switch entitlementFromEnv().kind {
		case entSigned:
			add("access", "ok", "signed tokens required (LANGSTROTH_RELAY_TOKEN_PUBKEY), workspace scope "+workspaceScopeFromEnv(), "")
		case entTokens:
			add("access", "ok", "static token allowlist (LANGSTROTH_RELAY_ACCESS_TOKENS)", "")
		default:
			add("access", "warn", "open: any client may use the relay",
				"before sharing a public URL, set LANGSTROTH_RELAY_TOKEN_PUBKEY (see `langstroth-relay help keygen`)")
		}
	}

	fromErr("tenancy", tenancyConfigError(), tenancyDetail(), "see docs/tenancy.md")
	fromErr("removed", evictionConfigError(), "no removed settings set",
		"unset LANGSTROTH_RELAY_MAX_ENVELOPES and LANGSTROTH_RELAY_RETENTION_DAYS (or set them to 0)")

	// Routes, tunnel and push.
	tunnel := strings.ToLower(strings.TrimSpace(getenv("LANGSTROTH_RELAY_TUNNEL")))
	if tunnel == "" {
		tunnel = TunnelOff
	}
	if err := routesConfigError(getenv); err != nil {
		add("routes", "fail", err.Error(), "set LANGSTROTH_RELAY_ROUTES to all, or turn the tunnel on")
	} else {
		r := strings.ToLower(strings.TrimSpace(getenv("LANGSTROTH_RELAY_ROUTES")))
		if r == "" {
			r = "all"
		}
		add("routes", "ok", "routes "+r+", tunnel "+tunnel, "")
	}
	if err := mailboxesConfigError(); err != nil {
		add("mailboxes", "fail", err.Error(), "set LANGSTROTH_RELAY_MAILBOXES to on, off or token (token needs an access gate); see `langstroth-relay help mailboxes`")
	} else if mode, _ := mailboxModeFromEnv(); mode == mailboxesOff {
		add("mailboxes", "skip", "approval mailboxes off (/v1/mailboxes/* answers 404)", "")
	} else {
		add("mailboxes", "ok", "approval mailboxes "+mode, "")
	}
	if err := apnsWithoutTunnelError(getenv); err != nil {
		add("push", "fail", err.Error(), "remove the APNs settings here, or turn the tunnel on")
	} else if tunnelOff(getenv) {
		add("push", "skip", "tunnel off, so no push", "")
	} else if s, err := APNsPushSenderFromEnv(); err != nil {
		add("push", "warn", err.Error()+"; the relay starts with push off", "see `langstroth-relay help push`")
	} else if s == nil {
		add("push", "skip", "APNs not configured; hosts fall back to ntfy if they have it", "")
	} else {
		add("push", "ok", "APNs push to "+strings.Join(s.Topics, ", ")+" ("+s.Env+")", "")
	}

	// Storage (no connection is made).
	dsn := getenv("DATABASE_URL")
	dir := getenv("LANGSTROTH_RELAY_DATA_DIR")
	switch {
	case envBool("LANGSTROTH_RELAY_REQUIRE_DB") && dsn == "":
		add("store", "fail", "LANGSTROTH_RELAY_REQUIRE_DB is set but DATABASE_URL is empty",
			"set DATABASE_URL (or DATABASE_URL_FILE), or unset LANGSTROTH_RELAY_REQUIRE_DB")
	case dsn != "":
		add("store", "ok", "Postgres (DATABASE_URL set; not contacted by check)", "")
	case dir != "":
		if err := dirWritable(dir); err != nil {
			add("store", "fail", "memory plus snapshot in "+dir+": "+err.Error(),
				"create the directory and make it writable by the relay's user")
		} else {
			add("store", "ok", "memory plus snapshot in "+filepath.Join(dir, snapshotFile), "")
		}
	default:
		add("store", "warn", "memory only: everything is lost on restart",
			"set LANGSTROTH_RELAY_DATA_DIR or DATABASE_URL to keep state")
	}

	backupCheck(add)

	if strings.TrimSpace(getenv("LANGSTROTH_RELAY_METRICS_TOKEN")) == "" {
		add("metrics", "warn", "/metrics is public", "set LANGSTROTH_RELAY_METRICS_TOKEN on a relay others can reach")
	} else {
		add("metrics", "ok", "/metrics needs the bearer token", "")
	}
	add("listen", "ok", resolveAddr(), "")

	if url != "" {
		probeRelay(strings.TrimRight(url, "/"), add)
	}
	// Commands to run next: the troubleshooting guide, or close the open gate.
	switch {
	case !rep.OK:
		rep.Next = append(rep.Next, "langstroth-relay help troubleshooting")
	case hasCheck(rep, "access", "warn"):
		rep.Next = append(rep.Next, "langstroth-relay keygen")
	}
	return rep
}

func hasCheck(rep checkReport, id, status string) bool {
	for _, c := range rep.Checks {
		if c.ID == id && c.Status == status {
			return true
		}
	}
	return false
}

func tenancyDetail() string {
	if strings.TrimSpace(getenv("LANGSTROTH_RELAY_TENANCY")) == "" {
		return "tenancy off"
	}
	return "tenancy config parses"
}

// dirWritable reports whether the relay could write its snapshot in dir.
// A missing directory is fine: the relay creates it on its first snapshot.
func dirWritable(dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// probeRelay checks a running relay's health and tunnel endpoints.
func probeRelay(base string, add func(name, status, detail, fix string)) {
	resp, err := checkHTTPClient.Get(base + "/v1/health")
	if err != nil {
		add("health", "fail", err.Error(), "is the relay running and the URL right?")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		add("health", "fail", fmt.Sprintf("GET /v1/health: %d %s", resp.StatusCode, strings.TrimSpace(string(body))),
			"503 means the store is unreachable: check DATABASE_URL and the database")
	} else {
		add("health", "ok", "GET /v1/health: 200 "+strings.TrimSpace(string(body)), "")
	}

	resp, err = checkHTTPClient.Get(base + "/v1/tunnel/info")
	if err != nil {
		add("tunnel-info", "fail", err.Error(), "is the relay running and the URL right?")
		return
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		add("tunnel-info", "skip", "tunnel off on this relay", "")
	case http.StatusOK:
		var info struct {
			Versions []int `json:"versions"`
			Push     bool  `json:"push"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<12)).Decode(&info); err != nil {
			add("tunnel-info", "warn", "GET /v1/tunnel/info: unreadable answer: "+err.Error(), "")
			return
		}
		add("tunnel-info", "ok", fmt.Sprintf("tunnel on, versions %v, push %t", info.Versions, info.Push), "")
	default:
		add("tunnel-info", "warn", fmt.Sprintf("GET /v1/tunnel/info: %d", resp.StatusCode), "")
	}
}
