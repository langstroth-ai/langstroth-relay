package relay

import (
	"errors"
	"fmt"
	"io/fs"
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
		v, err := read(filepath.Join(dir, canonical))
		if err == nil {
			return v
		}
		if !errors.Is(err, fs.ErrNotExist) {
			// Unreadable is not unset: secretFilesConfigError stops the
			// process at startup; this is for a file that changed later.
			warn(canonical+"@dir", "cannot read setting file", "var", canonical, "dir", secretsDirEnv, "err", err)
		}
	}
	return ""
}

// secretFilesConfigError refuses to start when a secret file the relay was
// pointed at can't be read or is empty: every <NAME>_FILE among the relay's
// settings (and DATABASE_URL_FILE), and every file in LIGUS_RELAY_SECRETS_DIR.
// getenv treats an unreadable file as unset, and for the access gate
// (LIGUS_RELAY_TOKEN_PUBKEY, LIGUS_RELAY_ACCESS_TOKENS) unset means open, for
// DATABASE_URL the in-memory store: a wrong file mode or a typo'd path must
// stop the relay instead.
func secretFilesConfigError(lookup func(string) string) error {
	var names []string
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok {
			names = append(names, k)
		}
	}
	return checkSecretFiles(lookup, names)
}

func checkSecretFiles(lookup func(string) string, names []string) error {
	seen := map[string]bool{}
	check := func(label, path string) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%s: cannot read %s: %w", label, path, err)
		}
		if strings.TrimSpace(string(b)) == "" {
			return fmt.Errorf("%s: %s is empty", label, path)
		}
		return nil
	}
	for _, k := range names {
		if seen[k] || !strings.HasSuffix(k, "_FILE") {
			continue
		}
		seen[k] = true
		if !strings.HasPrefix(k, envPrefix) && !strings.HasPrefix(k, legacyEnvPrefix) && k != "DATABASE_URL_FILE" {
			continue
		}
		if p := strings.TrimSpace(lookup(k)); p != "" {
			if err := check(k, p); err != nil {
				return err
			}
		}
	}
	dir := strings.TrimSpace(lookup(secretsDirEnv))
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", secretsDirEnv, err)
	}
	for _, e := range entries {
		// A mounted Secret's ..data / ..<timestamp> entries.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			continue
		}
		if err := check(secretsDirEnv+"/"+e.Name(), p); err != nil {
			return err
		}
	}
	return nil
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
