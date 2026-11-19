package relay

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fakeFlyctl serves `secrets list --json`, `ssh console -C 'printenv X'`,
// `secrets import --stage` (NAME=V and NAME="""...""") and `secrets unset
// --stage` from one file per secret in $FAKE_FLY_STATE, and logs every argv.
const fakeFlyctl = `#!/usr/bin/env bash
set -euo pipefail
S=$FAKE_FLY_STATE
printf '%s\n' "$*" >>"$S/argv"
case "$1 $2" in
"secrets list")
  first=1; printf '['
  for f in "$S"/sec/*; do
    [ -e "$f" ] || continue
    [ $first = 1 ] || printf ','; first=0
    printf '{"Name":"%s","Digest":"%s"}' "$(basename "$f")" "$(shasum -a 256 <"$f" | cut -c1-16)"
  done
  printf ']\n' ;;
"ssh console")
  name=${@: -1}; name=${name#printenv }
  [ -e "$S/sec/$name" ] && { cat "$S/sec/$name"; printf '\n'; } ;;
"secrets import")
  multi=""
  while IFS= read -r line; do
    if [ -n "$multi" ]; then
      if [ "$line" = '"""' ]; then printf '%s' "${buf%$'\n'}" >"$S/sec/$multi"; multi=""
      else buf+="$line"$'\n'; fi
    elif [[ "$line" == *'="""' ]]; then multi=${line%%=*}; buf=""
    else printf '%s' "${line#*=}" >"$S/sec/${line%%=*}"; fi
  done ;;
"secrets unset")
  for a in "$@"; do case "$a" in [A-Z]*) rm -f "$S/sec/$a" ;; esac; done ;;
*) echo "fake flyctl: unexpected $*" >&2; exit 9 ;;
esac
`

func TestFlyRenameSecrets(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "shasum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	dir := t.TempDir()
	fly := filepath.Join(dir, "flyctl")
	if err := os.WriteFile(fly, []byte(fakeFlyctl), 0o700); err != nil {
		t.Fatal(err)
	}
	sec := filepath.Join(dir, "sec")
	if err := os.Mkdir(sec, 0o700); err != nil {
		t.Fatal(err)
	}
	pem := "-----BEGIN PRIVATE KEY-----\nSECRETPEMLINE1\nSECRETPEMLINE2\n-----END PRIVATE KEY-----"
	// The secrets of the production app (names), with stand-in values.
	start := map[string]string{
		"DATABASE_URL":                 "postgres://SECRETDSN",
		"HIVE_RELAY_ADMIN_LOGINS":      "SECRETLOGINS",
		"HIVE_RELAY_MEMBERSHIP":        "1",
		"HIVE_RELAY_MEMBERSHIP_PUBKEY": "SECRETMEMBERPUB",
		"HIVE_RELAY_TUNNEL":            "open",
		"HIVE_RELAY_APNS_ENV":          "auto",
		"HIVE_RELAY_APNS_KEY":          pem,
		"HIVE_RELAY_APNS_KEY_ID":       "SECRETKEYID",
		"HIVE_RELAY_APNS_TEAM_ID":      "SECRETTEAM",
		"HIVE_RELAY_APNS_TOPIC":        "com.zazulam.hive.OLDTOPIC",
		"LIGUS_RELAY_APNS_TOPIC":       "com.zazulam.ligus.lab",
		// Removed in v0.3: renamed, it would stop the relay at startup
		// unless it is 0, so it is dropped.
		"HIVE_RELAY_MAX_ENVELOPES": "0",
	}
	for k, v := range start {
		if err := os.WriteFile(filepath.Join(sec, k), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) (string, error) {
		cmd := exec.Command("bash", append([]string{"deploy/fly-rename-secrets.sh", "hive-relay-enterprise"}, args...)...)
		cmd.Env = append(os.Environ(), "FLYCTL="+fly, "FAKE_FLY_STATE="+dir)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	noValues := func(where, s string) {
		t.Helper()
		if strings.Contains(s, "SECRET") || strings.Contains(s, "OLDTOPIC") || strings.Contains(s, "ligus.lab") || strings.Contains(s, "postgres://") {
			t.Fatalf("%s holds a secret value:\n%s", where, s)
		}
	}

	out, err := run()
	if err != nil {
		t.Fatalf("plan: %v\n%s", err, out)
	}
	noValues("plan output", out)
	if entries, _ := os.ReadDir(sec); len(entries) != len(start) {
		t.Fatal("plan mode changed secrets")
	}

	out, err = run("--apply")
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	noValues("apply output", out)
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	noValues("flyctl argv", string(argv))
	for _, l := range strings.Split(strings.TrimSpace(string(argv)), "\n") {
		if !strings.HasPrefix(l, "ssh console") && !strings.Contains(l, "--stage") && !strings.HasPrefix(l, "secrets list") {
			t.Fatalf("every change must be staged: %s", l)
		}
	}

	want := map[string]string{
		"DATABASE_URL":                       "postgres://SECRETDSN",
		"LANGSTROTH_RELAY_MEMBERSHIP":        "1",
		"LANGSTROTH_RELAY_MEMBERSHIP_PUBKEY": "SECRETMEMBERPUB",
		"LANGSTROTH_RELAY_TUNNEL":            "open",
		"LANGSTROTH_RELAY_APNS_ENV":          "auto",
		"LANGSTROTH_RELAY_APNS_KEY":          pem,
		"LANGSTROTH_RELAY_APNS_KEY_ID":       "SECRETKEYID",
		"LANGSTROTH_RELAY_APNS_TEAM_ID":      "SECRETTEAM",
		"LANGSTROTH_RELAY_APNS_TOPIC":        "com.zazulam.ligus.lab",
	}
	entries, _ := os.ReadDir(sec)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
		b, _ := os.ReadFile(filepath.Join(sec, e.Name()))
		if w, ok := want[e.Name()]; !ok || string(b) != w {
			t.Errorf("%s = %q after the rename, want %q (present: %v)", e.Name(), b, w, ok)
		}
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("secrets after the rename: %v", got)
	}

	// A second run finds nothing to do.
	if out, err = run("--apply"); err != nil || !strings.Contains(out, "no HIVE_RELAY_* or LIGUS_RELAY_* secrets") {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
}

// A rerun after a run that staged some new names and then stopped (a digest
// mismatch, say) must still apply the precedence rules: a HIVE_RELAY_ name
// whose LIGUS_RELAY_ twin exists is dropped, never compared against the
// LANGSTROTH_RELAY_ copy of the LIGUS_RELAY_ value.
func TestFlyRenameSecretsRerunAfterPartialRun(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "shasum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	dir := t.TempDir()
	fly := filepath.Join(dir, "flyctl")
	if err := os.WriteFile(fly, []byte(fakeFlyctl), 0o700); err != nil {
		t.Fatal(err)
	}
	sec := filepath.Join(dir, "sec")
	if err := os.Mkdir(sec, 0o700); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"HIVE_RELAY_APNS_TOPIC":        "com.zazulam.hive.OLDTOPIC",
		"LIGUS_RELAY_APNS_TOPIC":       "com.zazulam.ligus.lab",
		"LANGSTROTH_RELAY_APNS_TOPIC":  "com.zazulam.ligus.lab",
		"HIVE_RELAY_TUNNEL":            "open",
		"LANGSTROTH_RELAY_TUNNEL":      "open",
		"HIVE_RELAY_RETENTION_DAYS":    "0",
		"HIVE_RELAY_MEMBERSHIP_PUBKEY": "SECRETMEMBERPUB",
	} {
		if err := os.WriteFile(filepath.Join(sec, k), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "deploy/fly-rename-secrets.sh", "hive-relay-enterprise", "--apply")
	cmd.Env = append(os.Environ(), "FLYCTL="+fly, "FAKE_FLY_STATE="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	want := map[string]string{
		"LANGSTROTH_RELAY_APNS_TOPIC":        "com.zazulam.ligus.lab",
		"LANGSTROTH_RELAY_TUNNEL":            "open",
		"LANGSTROTH_RELAY_MEMBERSHIP_PUBKEY": "SECRETMEMBERPUB",
	}
	entries, _ := os.ReadDir(sec)
	if len(entries) != len(want) {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("secrets after the rerun: %v\n%s", names, out)
	}
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(sec, e.Name()))
		if want[e.Name()] != string(b) {
			t.Errorf("%s = %q, want %q", e.Name(), b, want[e.Name()])
		}
	}
}
