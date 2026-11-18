package langstrothauth

import (
	"path/filepath"
	"testing"
)

func TestAuthEnvAliases(t *testing.T) {
	t.Setenv("LIGUS_AUTH_ENV_ALIAS_TEST", "former")
	if got := authGetenv("LANGSTROTH_AUTH_ENV_ALIAS_TEST"); got != "former" {
		t.Fatalf("former name must keep working, got %q", got)
	}
	if err := envConflictError(); err != nil {
		t.Fatalf("former name alone: %v", err)
	}
	t.Setenv("LANGSTROTH_AUTH_ENV_ALIAS_TEST", "former")
	if err := envConflictError(); err != nil {
		t.Fatalf("both names agreeing: %v", err)
	}
	t.Setenv("LANGSTROTH_AUTH_ENV_ALIAS_TEST", "new")
	if got := authGetenv("LIGUS_AUTH_ENV_ALIAS_TEST"); got != "new" {
		t.Fatalf("canonical name must win, got %q", got)
	}
	if err := envConflictError(); err == nil {
		t.Fatal("both names with different values must stop the service")
	}
}

func TestSigningKeyFromFormerEnvAndCredential(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteNewSigningKey(filepath.Join(dir, "ligus-auth-signing-key")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LANGSTROTH_AUTH_SIGNING_KEY_FILE", "")
	t.Setenv("LIGUS_AUTH_SIGNING_KEY_FILE", "")
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	if _, src, err := LoadSigningKey(""); err != nil || src != "file" {
		t.Fatalf("former systemd credential name: %q %v", src, err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	t.Setenv("LIGUS_AUTH_SIGNING_KEY_FILE", filepath.Join(dir, "ligus-auth-signing-key"))
	if _, src, err := LoadSigningKey(""); err != nil || src != "file" {
		t.Fatalf("former LIGUS_AUTH_SIGNING_KEY_FILE: %q %v", src, err)
	}
}
