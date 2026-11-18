// Package envalias refuses environment variables under prefixes that are no
// longer read. The relay's settings are LANGSTROTH_RELAY_* and the auth
// service's LANGSTROTH_AUTH_*; the names from before the Langstroth rename
// (HIVE_RELAY_*, LIGUS_RELAY_*, LIGUS_AUTH_*) are ignored, so a secret left
// under one of them would silently turn its setting off (push, the tunnel,
// the access gate). The process refuses to start instead.
package envalias

import (
	"fmt"
	"sort"
	"strings"
)

// Removed returns an error naming every variable in environ (os.Environ()
// form; only the names are used, never the values) that starts with one of
// the removed prefixes, each with the name to use instead. Names in exempt
// are skipped (another program's variables that share a prefix).
func Removed(environ []string, canonical string, removed []string, exempt map[string]bool) error {
	var found []string
	seen := map[string]bool{}
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if seen[k] || exempt[k] {
			continue
		}
		seen[k] = true
		for _, p := range removed {
			if short, ok := strings.CutPrefix(k, p); ok {
				found = append(found, k+" -> "+canonical+short)
				break
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return fmt.Errorf("%s* names are no longer read: rename %s", strings.Join(removed, "* and "), strings.Join(found, ", "))
}
