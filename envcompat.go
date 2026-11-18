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

	"github.com/langstroth-ai/langstroth-relay/internal/envalias"
)

// Environment-variable compatibility for the Hive -> Ligus -> Langstroth
// renames.
//
// The relay's settings are canonically named LANGSTROTH_RELAY_*; the former
// LIGUS_RELAY_* names and the legacy HIVE_RELAY_* names are still accepted.
// Call sites keep naming the legacy key (e.g. getenv("HIVE_RELAY_ADDR")) and
// getenv resolves it:
//
//  1. LANGSTROTH_RELAY_<X> if set (non-empty) wins. Main refuses to start
//     when it and a former name for the same setting are both set to
//     different values (envAliasConflictError);
//  2. otherwise LIGUS_RELAY_<X>; if HIVE_RELAY_<X> is also set to a different
//     value, LIGUS wins and a one-time conflict warning is logged;
//  3. otherwise HIVE_RELAY_<X>, with a one-time deprecation warning per
//     variable.
//
// Names outside these prefixes (PORT, DATABASE_URL, ...) are read as-is. The
// former names stay until deployed secrets have been migrated; removing them
// early would leave a relay fail-closed.

const (
	envPrefix       = "LANGSTROTH_RELAY_"
	formerEnvPrefix = "LIGUS_RELAY_"
	legacyEnvPrefix = "HIVE_RELAY_"
)

// envPrefixes lists the prefixes of one setting, canonical first.
var envPrefixes = []string{envPrefix, formerEnvPrefix, legacyEnvPrefix}

// settingSuffix returns the part of name after any of envPrefixes.
func settingSuffix(name string) (string, bool) {
	for _, p := range envPrefixes {
		if strings.HasPrefix(name, p) {
			return strings.TrimPrefix(name, p), true
		}
	}
	return "", false
}

// envAliasConflictError refuses to start when a LANGSTROTH_RELAY_* setting and
// its LIGUS_RELAY_* or HIVE_RELAY_* name are both set to different values.
func envAliasConflictError(environ []string, lookup func(string) string) error {
	return envalias.Conflict(environ, lookup, envPrefixes...)
}

var envWarned sync.Map // legacy name -> struct{}

// getenv returns the value for a relay setting, preferring LANGSTROTH_RELAY_*
// over the former LIGUS_RELAY_* and legacy HIVE_RELAY_* names. See the file
// comment.
//
// When the variable itself is unset, a setting can come from a file (for
// Kubernetes Secrets mounted as files, so secrets stay out of the process
// environment):
//
//  1. the file named by <NAME>_FILE (LANGSTROTH_RELAY_<X>_FILE, or a former
//     LIGUS_RELAY_<X>_FILE / HIVE_RELAY_<X>_FILE; DATABASE_URL_FILE for
//     unprefixed names);
//  2. the file $LANGSTROTH_RELAY_SECRETS_DIR/<NAME>, with NAME in its
//     canonical spelling (LANGSTROTH_RELAY_<X>, DATABASE_URL, ...), else in
//     its former spelling (LIGUS_RELAY_<X>) for directories written before
//     the rename (checkSecretFiles refuses two files that disagree).
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
	if s, ok := settingSuffix(name); ok {
		short = s
		canonical = envPrefix + short
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
	dir := strings.TrimSpace(resolveEnv(secretsDirEnv, lookup, warn))
	if dir == "" || short == "SECRETS_DIR" || strings.ContainsAny(canonical, "/\\") {
		return ""
	}
	names := []string{canonical}
	if short != "" {
		names = append(names, formerEnvPrefix+short)
	}
	for _, n := range names {
		v, err := read(filepath.Join(dir, n))
		if err == nil {
			return v
		}
		if !errors.Is(err, fs.ErrNotExist) {
			// Unreadable is not unset: secretFilesConfigError stops the
			// process at startup; this is for a file that changed later.
			warn(n+"@dir", "cannot read setting file", "var", n, "dir", secretsDirEnv, "err", err)
			return ""
		}
	}
	return ""
}

// secretFilesConfigError refuses to start when a secret file the relay was
// pointed at can't be read or is empty: every <NAME>_FILE among the relay's
// settings (and DATABASE_URL_FILE), and every file in
// LANGSTROTH_RELAY_SECRETS_DIR. getenv treats an unreadable file as unset, and
// for the access gate (LANGSTROTH_RELAY_TOKEN_PUBKEY,
// LANGSTROTH_RELAY_ACCESS_TOKENS) unset means open, for DATABASE_URL the
// in-memory store: a wrong file mode or a typo'd path must stop the relay
// instead. It also refuses a directory holding both LANGSTROTH_RELAY_<X> and
// LIGUS_RELAY_<X> with different contents.
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
		if _, ok := settingSuffix(k); !ok && k != "DATABASE_URL_FILE" {
			continue
		}
		if p := strings.TrimSpace(lookup(k)); p != "" {
			if err := check(k, p); err != nil {
				return err
			}
		}
	}
	dir := strings.TrimSpace(resolveEnv(secretsDirEnv, lookup, func(string, string, ...any) {}))
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
		if short, ok := strings.CutPrefix(e.Name(), envPrefix); ok {
			former := filepath.Join(dir, formerEnvPrefix+short)
			if b, err := os.ReadFile(former); err == nil {
				cur, _ := readSecretFile(p)
				if strings.TrimRight(string(b), " \t\r\n") != cur {
					return fmt.Errorf("%s: %s and %s hold different values: keep only %s", secretsDirEnv, e.Name(), formerEnvPrefix+short, e.Name())
				}
			}
		}
	}
	return nil
}

func resolveEnv(name string, lookup func(string) string, warn func(key, msg string, args ...any)) string {
	short, ok := settingSuffix(name)
	if !ok {
		return lookup(name)
	}
	canonical, former, legacy := envPrefix+short, formerEnvPrefix+short, legacyEnvPrefix+short
	newV, formerV, oldV := lookup(canonical), lookup(former), lookup(legacy)
	switch {
	case newV != "":
		if (formerV != "" && formerV != newV) || (oldV != "" && oldV != newV) {
			// Main refuses to start on this (envAliasConflictError); an
			// embedding program gets the canonical value and a warning.
			warn(canonical, "env var set under its new and a former name with different values; using the new name", "new", canonical)
		}
		return newV
	case formerV != "":
		if oldV != "" && oldV != formerV {
			warn(legacy, "both env vars are set with different values; using the newer name", "new", former, "old", legacy)
		}
		return formerV
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
