package envalias

import (
	"strings"
	"testing"
)

func TestConflict(t *testing.T) {
	prefixes := []string{"LANGSTROTH_RELAY_", "LIGUS_RELAY_", "HIVE_RELAY_"}
	for _, tc := range []struct {
		name string
		env  map[string]string
		bad  string
	}{
		{"none", map[string]string{}, ""},
		{"canonical only", map[string]string{"LANGSTROTH_RELAY_ADDR": "a"}, ""},
		{"former only", map[string]string{"LIGUS_RELAY_ADDR": "a", "HIVE_RELAY_ADDR": "b"}, ""},
		{"agree", map[string]string{"LANGSTROTH_RELAY_ADDR": "a", "LIGUS_RELAY_ADDR": "a", "HIVE_RELAY_ADDR": "a"}, ""},
		{"empty former is unset", map[string]string{"LANGSTROTH_RELAY_ADDR": "a", "LIGUS_RELAY_ADDR": ""}, ""},
		{"empty canonical is unset", map[string]string{"LANGSTROTH_RELAY_ADDR": "", "LIGUS_RELAY_ADDR": "b"}, ""},
		{"ligus differs", map[string]string{"LANGSTROTH_RELAY_ADDR": "a", "LIGUS_RELAY_ADDR": "b"}, "LIGUS_RELAY_ADDR"},
		{"hive differs", map[string]string{"LANGSTROTH_RELAY_TUNNEL": "open", "HIVE_RELAY_TUNNEL": "off"}, "HIVE_RELAY_TUNNEL"},
		{"file names too", map[string]string{"LANGSTROTH_RELAY_ACCESS_TOKENS_FILE": "/a", "LIGUS_RELAY_ACCESS_TOKENS_FILE": "/b"}, "LIGUS_RELAY_ACCESS_TOKENS_FILE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var environ []string
			for k, v := range tc.env {
				environ = append(environ, k+"="+v)
			}
			err := Conflict(environ, func(k string) string { return tc.env[k] }, prefixes...)
			if tc.bad == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.bad) {
				t.Fatalf("want an error naming %s, got %v", tc.bad, err)
			}
		})
	}
}
