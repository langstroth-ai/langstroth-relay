package relay

import (
	"log/slog"
	"os"
	"path/filepath"
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
//
// When the variable itself is unset, a setting can come from a file (for
// Kubernetes Secrets mounted as files, so secrets stay out of the process
// environment):
//
//  1. the file named by <NAME>_FILE (LIGUS_RELAY_<X>_FILE, or the legacy
//     HIVE_RELAY_<X>_FILE; DATABASE_URL_FILE for unprefixed names);
//  2. the file $LIGUS_RELAY_SECRETS_DIR/<NAME>, with NAME in its canonical
//     spelling (LIGUS_RELAY_<X>, DATABASE_URL, ...).
//
// The environment wins over a _FILE, which wins over the directory. One
// trailing run of whitespace is trimmed. Names that already end in _FILE are
// never resolved from files themselves, and settings with their own _FILE
// meaning (APNS_KEY, whose APNS_KEY_FILE the relay opens itself) skip step 1.
func getenv(name string) string {
	return resolveSetting(name, os.Getenv, readSecretFile, warnEnvOnce)
}

// secretsDirEnv names the directory of one-file-per-setting secrets.
const secretsDirEnv = envPrefix + "SECRETS_DIR"

// noFileFallback lists settings whose <NAME>_FILE already means something
// else (a path the relay reads itself).
var noFileFallback = map[string]bool{"APNS_KEY": true}

func readSecretFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), " \t\r\n"), nil
}

// resolveSetting is resolveEnv plus the file fallbacks described on getenv.
func resolveSetting(name string, lookup func(string) string, read func(string) (string, error), warn func(key, msg string, args ...any)) string {
	if v := resolveEnv(name, lookup, warn); v != "" || strings.HasSuffix(name, "_FILE") {
		return v
	}
	canonical, short := name, ""
	switch {
	case strings.HasPrefix(name, legacyEnvPrefix):
		short = strings.TrimPrefix(name, legacyEnvPrefix)
		canonical = envPrefix + short
	case strings.HasPrefix(name, envPrefix):
		short = strings.TrimPrefix(name, envPrefix)
	}
	if !noFileFallback[short] {
		if path := strings.TrimSpace(resolveEnv(name+"_FILE", lookup, warn)); path != "" {
			v, err := read(path)
			if err != nil {
				// Never echo the contents; the path is fine to log.
				warn(name+"_FILE", "cannot read setting file", "var", canonical+"_FILE", "err", err)
				return ""
			}
			return v
		}
	}
	if dir := strings.TrimSpace(lookup(secretsDirEnv)); dir != "" && canonical != secretsDirEnv && !strings.ContainsAny(canonical, "/\\") {
		if v, err := read(filepath.Join(dir, canonical)); err == nil {
			return v
		}
	}
	return ""
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
