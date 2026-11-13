package relay

import (
	"log/slog"
	"os"
	"strings"
	"sync"
)

// Environment-variable compatibility for the Hive -> Ligus rename.
//
// The relay's settings are canonically named LIGUS_RELAY_*; the legacy
// HIVE_RELAY_* names are still accepted. Call sites keep naming the legacy
// key (e.g. getenv("HIVE_RELAY_ADDR")) and getenv resolves it:
//
//  1. LIGUS_RELAY_<X> if set (non-empty) wins;
//  2. otherwise HIVE_RELAY_<X>, with a one-time deprecation warning per
//     variable;
//  3. if both are set to different values, LIGUS wins and a one-time conflict
//     warning is logged.
//
// Names outside the HIVE_RELAY_ prefix (PORT, DATABASE_URL, ...) are read
// as-is. The HIVE_RELAY_* fallback stays until deployed secrets have been
// migrated; removing it early would leave a relay fail-closed.

const (
	legacyEnvPrefix = "HIVE_RELAY_"
	envPrefix       = "LIGUS_RELAY_"
)

var envWarned sync.Map // legacy name -> struct{}

// getenv returns the value for a relay setting, preferring LIGUS_RELAY_* over
// the legacy HIVE_RELAY_* name. See the file comment.
func getenv(name string) string {
	return resolveEnv(name, os.Getenv, warnEnvOnce)
}

func resolveEnv(name string, lookup func(string) string, warn func(key, msg string, args ...any)) string {
	var legacy, canonical string
	switch {
	case strings.HasPrefix(name, legacyEnvPrefix):
		legacy, canonical = name, envPrefix+strings.TrimPrefix(name, legacyEnvPrefix)
	case strings.HasPrefix(name, envPrefix):
		legacy, canonical = legacyEnvPrefix+strings.TrimPrefix(name, envPrefix), name
	default:
		return lookup(name)
	}
	newV, oldV := lookup(canonical), lookup(legacy)
	switch {
	case newV != "" && oldV != "" && newV != oldV:
		warn(legacy, "both env vars are set with different values; using the new name", "new", canonical, "old", legacy)
		return newV
	case newV != "":
		return newV
	case oldV != "":
		warn(legacy, "deprecated env var name; rename it", "old", legacy, "new", canonical)
		return oldV
	}
	return ""
}

func warnEnvOnce(key, msg string, args ...any) {
	if _, loaded := envWarned.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	slog.Warn(msg, args...)
}
