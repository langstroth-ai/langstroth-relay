package relay

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveEnv(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		key       string
		want      string
		wantWarns int
	}{
		{"new only", map[string]string{"LIGUS_RELAY_ADDR": "a"}, "HIVE_RELAY_ADDR", "a", 0},
		{"old only warns", map[string]string{"HIVE_RELAY_ADDR": "b"}, "HIVE_RELAY_ADDR", "b", 1},
		{"both equal", map[string]string{"LIGUS_RELAY_ADDR": "c", "HIVE_RELAY_ADDR": "c"}, "HIVE_RELAY_ADDR", "c", 0},
		{"both differ new wins", map[string]string{"LIGUS_RELAY_ADDR": "new", "HIVE_RELAY_ADDR": "old"}, "HIVE_RELAY_ADDR", "new", 1},
		{"empty new falls back", map[string]string{"LIGUS_RELAY_ADDR": "", "HIVE_RELAY_ADDR": "d"}, "HIVE_RELAY_ADDR", "d", 1},
		{"canonical key accepted", map[string]string{"HIVE_RELAY_TUNNEL": "open"}, "LIGUS_RELAY_TUNNEL", "open", 1},
		{"langstroth only", map[string]string{"LANGSTROTH_RELAY_ADDR": "l"}, "HIVE_RELAY_ADDR", "l", 0},
		{"langstroth key accepted", map[string]string{"LIGUS_RELAY_TUNNEL": "open"}, "LANGSTROTH_RELAY_TUNNEL", "open", 0},
		{"langstroth wins over ligus and hive", map[string]string{"LANGSTROTH_RELAY_ADDR": "l", "LIGUS_RELAY_ADDR": "l", "HIVE_RELAY_ADDR": "l"}, "LIGUS_RELAY_ADDR", "l", 0},
		{"langstroth conflict warns", map[string]string{"LANGSTROTH_RELAY_ADDR": "l", "LIGUS_RELAY_ADDR": "x"}, "HIVE_RELAY_ADDR", "l", 1},
		{"empty langstroth falls back", map[string]string{"LANGSTROTH_RELAY_ADDR": "", "LIGUS_RELAY_ADDR": "x"}, "HIVE_RELAY_ADDR", "x", 0},
		{"neither", map[string]string{}, "HIVE_RELAY_ADDR", "", 0},
		{"unprefixed passthrough", map[string]string{"PORT": "9"}, "PORT", "9", 0},
		{"unprefixed ignores LIGUS", map[string]string{"LIGUS_RELAY_PORT": "x"}, "PORT", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warns := 0
			got := resolveEnv(tc.key, func(k string) string { return tc.env[k] }, func(string, string, ...any) { warns++ })
			if got != tc.want || warns != tc.wantWarns {
				t.Fatalf("resolveEnv(%q) = %q (%d warns), want %q (%d warns)", tc.key, got, warns, tc.want, tc.wantWarns)
			}
		})
	}
}

func TestGetenvPrefersLigusAndWarnsOnce(t *testing.T) {
	key := fmt.Sprintf("HIVE_RELAY_TEST_ONCE_%p", t)
	t.Setenv(key, "legacy")
	if got := getenv(key); got != "legacy" {
		t.Fatalf("fallback = %q", got)
	}
	if _, ok := envWarned.Load(key); !ok {
		t.Fatal("expected a deprecation warning to be recorded")
	}
	// Second read must not panic or re-log; the marker stays set.
	_ = getenv(key)
	t.Setenv("LIGUS_RELAY_"+key[len("HIVE_RELAY_"):], "new")
	if got := getenv(key); got != "new" {
		t.Fatalf("LIGUS name should win, got %q", got)
	}
}

// Real settings go through getenv: a LIGUS_RELAY_* name drives behaviour.
func TestLigusEnvDrivesSettings(t *testing.T) {
	t.Setenv("LIGUS_RELAY_MAX_BODY_BYTES", "1234")
	if got := envInt64("HIVE_RELAY_MAX_BODY_BYTES", 1); got != 1234 {
		t.Fatalf("envInt64 via LIGUS = %d", got)
	}
	t.Setenv("LIGUS_RELAY_INVITE_REQUIRE_FRIEND", "1")
	if !envBool("HIVE_RELAY_INVITE_REQUIRE_FRIEND") {
		t.Fatal("envBool via LIGUS = false")
	}
}

func TestResolveSettingFiles(t *testing.T) {
	files := map[string]string{
		"/run/db":                           "postgres://from-file",
		"/run/token":                        "tok",
		"/s/LIGUS_RELAY_METRICS_TOKEN":      "from-dir",
		"/s/DATABASE_URL":                   "postgres://from-dir",
		"/s/LIGUS_RELAY_ACCESS_TOKENS":      "dir-tokens",
		"/s/LIGUS_RELAY_APNS_KEY":           "pem-from-dir",
		"/n/LANGSTROTH_RELAY_METRICS_TOKEN": "new-dir",
		"/n/LIGUS_RELAY_METRICS_TOKEN":      "former-dir",
	}
	read := func(p string) (string, error) {
		if v, ok := files[p]; ok {
			return v, nil
		}
		return "", fmt.Errorf("open %s: %w", p, fs.ErrNotExist)
	}
	cases := []struct {
		name string
		env  map[string]string
		key  string
		want string
	}{
		{"env wins over file", map[string]string{"DATABASE_URL": "env", "DATABASE_URL_FILE": "/run/db"}, "DATABASE_URL", "env"},
		{"unprefixed _FILE", map[string]string{"DATABASE_URL_FILE": "/run/db"}, "DATABASE_URL", "postgres://from-file"},
		{"canonical _FILE via legacy key", map[string]string{"LIGUS_RELAY_METRICS_TOKEN_FILE": "/run/token"}, "HIVE_RELAY_METRICS_TOKEN", "tok"},
		{"legacy _FILE", map[string]string{"HIVE_RELAY_METRICS_TOKEN_FILE": "/run/token"}, "LIGUS_RELAY_METRICS_TOKEN", "tok"},
		{"_FILE wins over dir", map[string]string{"LIGUS_RELAY_METRICS_TOKEN_FILE": "/run/token", "LIGUS_RELAY_SECRETS_DIR": "/s"}, "HIVE_RELAY_METRICS_TOKEN", "tok"},
		{"dir by former name", map[string]string{"LIGUS_RELAY_SECRETS_DIR": "/s"}, "HIVE_RELAY_METRICS_TOKEN", "from-dir"},
		{"dir named by the canonical env var", map[string]string{"LANGSTROTH_RELAY_SECRETS_DIR": "/s"}, "LANGSTROTH_RELAY_METRICS_TOKEN", "from-dir"},
		{"dir prefers the canonical file name", map[string]string{"LANGSTROTH_RELAY_SECRETS_DIR": "/n"}, "HIVE_RELAY_METRICS_TOKEN", "new-dir"},
		{"canonical _FILE", map[string]string{"LANGSTROTH_RELAY_METRICS_TOKEN_FILE": "/run/token"}, "HIVE_RELAY_METRICS_TOKEN", "tok"},
		{"dir for unprefixed", map[string]string{"LIGUS_RELAY_SECRETS_DIR": "/s"}, "DATABASE_URL", "postgres://from-dir"},
		{"missing file in dir", map[string]string{"LIGUS_RELAY_SECRETS_DIR": "/s"}, "LIGUS_RELAY_ADDR", ""},
		{"unreadable _FILE is empty", map[string]string{"DATABASE_URL_FILE": "/nope"}, "DATABASE_URL", ""},
		{"_FILE names never recurse", map[string]string{"LIGUS_RELAY_SECRETS_DIR": "/s"}, "LIGUS_RELAY_APNS_KEY_FILE", ""},
		{"APNS_KEY skips its own _FILE", map[string]string{"LIGUS_RELAY_APNS_KEY_FILE": "/run/token"}, "HIVE_RELAY_APNS_KEY", ""},
		{"APNS_KEY still reads the dir", map[string]string{"LIGUS_RELAY_SECRETS_DIR": "/s"}, "HIVE_RELAY_APNS_KEY", "pem-from-dir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveSetting(tc.key, func(k string) string { return tc.env[k] }, read, func(string, string, ...any) {})
			if got != tc.want {
				t.Fatalf("resolveSetting(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

func TestGetenvReadsRealFileAndTrims(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "LIGUS_RELAY_FILE_TEST_SETTING"), []byte("v1\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGUS_RELAY_SECRETS_DIR", dir)
	if got := getenv("HIVE_RELAY_FILE_TEST_SETTING"); got != "v1" {
		t.Fatalf("got %q", got)
	}
}

// Main refuses to start when one setting has two different values under its
// LANGSTROTH_RELAY_* and a former name.
func TestEnvAliasConflictError(t *testing.T) {
	env := map[string]string{"LANGSTROTH_RELAY_TUNNEL": "open", "LIGUS_RELAY_TUNNEL": "off"}
	environ := []string{"LANGSTROTH_RELAY_TUNNEL=open", "LIGUS_RELAY_TUNNEL=off"}
	if err := envAliasConflictError(environ, func(k string) string { return env[k] }); err == nil {
		t.Fatal("conflicting LANGSTROTH/LIGUS values must stop the relay")
	}
	env["LIGUS_RELAY_TUNNEL"] = "open"
	env["HIVE_RELAY_TUNNEL"] = "off"
	environ = append(environ, "HIVE_RELAY_TUNNEL=off")
	if err := envAliasConflictError(environ, func(k string) string { return env[k] }); err == nil {
		t.Fatal("conflicting LANGSTROTH/HIVE values must stop the relay")
	}
	env["HIVE_RELAY_TUNNEL"] = "open"
	if err := envAliasConflictError(environ, func(k string) string { return env[k] }); err != nil {
		t.Fatalf("agreeing values: %v", err)
	}
	// LIGUS vs HIVE alone keeps its old meaning: LIGUS wins with a warning.
	only := map[string]string{"LIGUS_RELAY_TUNNEL": "open", "HIVE_RELAY_TUNNEL": "off"}
	if err := envAliasConflictError([]string{"LIGUS_RELAY_TUNNEL=open", "HIVE_RELAY_TUNNEL=off"}, func(k string) string { return only[k] }); err != nil {
		t.Fatalf("LIGUS vs HIVE must not stop the relay: %v", err)
	}
}

// A secrets directory written before the rename (LIGUS_RELAY_* file names)
// still works; one that holds both spellings must agree.
func TestCheckSecretFilesDirAliases(t *testing.T) {
	dir := t.TempDir()
	write := func(n, v string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, n), []byte(v), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	write("LIGUS_RELAY_ACCESS_TOKENS", "a\n")
	lookup := func(k string) string {
		if k == "LANGSTROTH_RELAY_SECRETS_DIR" {
			return dir
		}
		return ""
	}
	if err := checkSecretFiles(lookup, nil); err != nil {
		t.Fatalf("former names only: %v", err)
	}
	write("LANGSTROTH_RELAY_ACCESS_TOKENS", "a")
	if err := checkSecretFiles(lookup, nil); err != nil {
		t.Fatalf("same value under both names: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "LANGSTROTH_RELAY_ACCESS_TOKENS")); err != nil {
		t.Fatal(err)
	}
	write("LANGSTROTH_RELAY_ACCESS_TOKENS", "b")
	if err := checkSecretFiles(lookup, nil); err == nil {
		t.Fatal("different values under both names must stop the relay")
	}
}
