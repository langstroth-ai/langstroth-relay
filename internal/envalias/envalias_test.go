package envalias

import (
	"strings"
	"testing"
)

func TestRemoved(t *testing.T) {
	removed := []string{"HIVE_RELAY_", "LIGUS_RELAY_"}
	exempt := map[string]bool{"LIGUS_RELAY_URL": true}
	for _, tc := range []struct {
		name    string
		environ []string
		want    []string
	}{
		{"none", nil, nil},
		{"canonical only", []string{"LANGSTROTH_RELAY_ADDR=a", "PORT=1"}, nil},
		{"exempt", []string{"LIGUS_RELAY_URL=http://x"}, nil},
		{"hive", []string{"HIVE_RELAY_TUNNEL=open"}, []string{"HIVE_RELAY_TUNNEL -> LANGSTROTH_RELAY_TUNNEL"}},
		{"empty still counts", []string{"HIVE_RELAY_APNS_KEY="}, []string{"HIVE_RELAY_APNS_KEY -> LANGSTROTH_RELAY_APNS_KEY"}},
		{"all listed", []string{"LIGUS_RELAY_APNS_TOPIC=t", "HIVE_RELAY_MEMBERSHIP=1"}, []string{"HIVE_RELAY_MEMBERSHIP -> LANGSTROTH_RELAY_MEMBERSHIP", "LIGUS_RELAY_APNS_TOPIC -> LANGSTROTH_RELAY_APNS_TOPIC"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Removed(tc.environ, "LANGSTROTH_RELAY_", removed, exempt)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("error %q does not name %q", err, w)
				}
			}
		})
	}
}

func TestRemovedNeverEchoesValues(t *testing.T) {
	err := Removed([]string{"HIVE_RELAY_APNS_KEY=-----BEGIN PRIVATE KEY-----secret"}, "LANGSTROTH_RELAY_", []string{"HIVE_RELAY_"}, nil)
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "BEGIN") {
		t.Fatalf("error must name the variable without its value: %v", err)
	}
}
