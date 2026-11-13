package relay

import (
	"fmt"
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
