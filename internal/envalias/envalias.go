// Package envalias checks environment variables that are accepted under more
// than one prefix (the Langstroth rename: LANGSTROTH_RELAY_* is canonical,
// LIGUS_RELAY_* and HIVE_RELAY_* are former names of the same settings).
package envalias

import (
	"fmt"
	"sort"
	"strings"
)

// Conflict returns an error when a variable under the canonical prefix
// (prefixes[0]) and the same variable under a former prefix (prefixes[1:])
// are both set, non-empty, to different values. environ is os.Environ()
// (only the names are used); lookup reads a value. A process that starts
// with two different values for one setting would silently pick one of them,
// so the caller refuses to start instead.
func Conflict(environ []string, lookup func(string) string, prefixes ...string) error {
	if len(prefixes) < 2 {
		return nil
	}
	canonical := prefixes[0]
	var shorts []string
	seen := map[string]bool{}
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(k, canonical) || seen[k] {
			continue
		}
		seen[k] = true
		shorts = append(shorts, strings.TrimPrefix(k, canonical))
	}
	sort.Strings(shorts)
	for _, short := range shorts {
		v := lookup(canonical + short)
		if v == "" {
			continue
		}
		for _, p := range prefixes[1:] {
			if old := lookup(p + short); old != "" && old != v {
				return fmt.Errorf("%s%s and %s%s are both set, to different values: set only %s%s", canonical, short, p, short, canonical, short)
			}
		}
	}
	return nil
}
