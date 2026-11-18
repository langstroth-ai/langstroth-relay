package langstrothauth

import (
	"os"
	"strings"

	"github.com/langstroth-ai/langstroth-relay/internal/envalias"
)

// Environment-variable names. LANGSTROTH_AUTH_* is canonical; the former
// LIGUS_AUTH_* names (before the Langstroth rename) keep working. Code names
// the canonical spelling; authEnvName finds whichever spelling is set.
// Both set to different values stops the service at startup
// (envConflictError).
const (
	authEnvPrefix       = "LANGSTROTH_AUTH_"
	formerAuthEnvPrefix = "LIGUS_AUTH_"
)

// authEnvName returns the variable to read for name (either spelling): the
// canonical one if it is non-empty, else the former one if that is non-empty,
// else whichever is set but empty (so readSecret reports it), else "".
func authEnvName(name string) string {
	short, ok := strings.CutPrefix(name, authEnvPrefix)
	if !ok {
		short, ok = strings.CutPrefix(name, formerAuthEnvPrefix)
	}
	if !ok {
		if _, set := os.LookupEnv(name); set {
			return name
		}
		return ""
	}
	names := []string{authEnvPrefix + short, formerAuthEnvPrefix + short}
	for _, n := range names {
		if os.Getenv(n) != "" {
			return n
		}
	}
	for _, n := range names {
		if _, set := os.LookupEnv(n); set {
			return n
		}
	}
	return ""
}

// authGetenv reads name under its canonical or former spelling.
func authGetenv(name string) string {
	if n := authEnvName(name); n != "" {
		return os.Getenv(n)
	}
	return ""
}

// envConflictError refuses to start when a LANGSTROTH_AUTH_* variable and its
// LIGUS_AUTH_* name are both set to different values.
func envConflictError() error {
	return envalias.Conflict(os.Environ(), os.Getenv, authEnvPrefix, formerAuthEnvPrefix)
}
