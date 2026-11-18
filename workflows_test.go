package relay

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Supply-chain guard for .github/workflows and the Fly deploy. These are
// plain-text checks (no YAML dependency) over files whose shape we control.

func readRepoFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var usesRe = regexp.MustCompile(`(?m)^\s*-?\s*uses:\s*(\S+)`)
var shaPinRe = regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40}$`)

func TestWorkflows_PinnedAndLeastPrivilege(t *testing.T) {
	files, err := filepath.Glob(".github/workflows/*.yml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	for _, f := range files {
		src := readRepoFile(t, f)
		if !regexp.MustCompile(`(?m)^permissions:`).MatchString(src) {
			t.Errorf("%s: no top-level permissions block (the token gets the repo default scope)", f)
		}
		if strings.Contains(src, "pull_request_target") {
			t.Errorf("%s: pull_request_target runs fork code with a privileged token", f)
		}
		if regexp.MustCompile(`(?m)^\s*\w+:\s*write`).MatchString(src) {
			t.Errorf("%s: grants a write scope; no relay workflow needs one", f)
		}
		// bash -e does not abort on a failing "! cmd" unless it is the last
		// command, so a negative check written that way is silently skipped.
		for i, line := range strings.Split(src, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "! ") {
				t.Errorf("%s:%d: negative check %q does not trip errexit; use if ...; then exit 1; fi", f, i+1, strings.TrimSpace(line))
			}
		}
		for _, m := range usesRe.FindAllStringSubmatch(src, -1) {
			if !shaPinRe.MatchString(m[1]) {
				t.Errorf("%s: action %q is not pinned to a full commit SHA", f, m[1])
			}
		}
	}
}

// The production deploy must build the OPEN binary from the repo-root
// Dockerfile, deploy exactly the commit CI tested, only from main, and never
// configure a token gate (which would make the public relay 401 every client).
func TestDeployWorkflow_OnlyTestedOpenRelayFromMain(t *testing.T) {
	src := readRepoFile(t, ".github/workflows/deploy.yml")

	if !strings.Contains(src, "ref: ${{ github.event.workflow_run.head_sha || github.sha }}") {
		t.Error("checkout must pin the commit CI tested (workflow_run.head_sha); the default ref is whatever main is at deploy time")
	}
	if !strings.Contains(src, "github.ref == 'refs/heads/main'") {
		t.Error("a manual dispatch must be limited to main, or any branch can be deployed to the public relay")
	}
	if strings.Contains(src, "setup-flyctl") {
		t.Error("setup-flyctl installs whatever flyctl the Fly API names, unverified; install a pinned, checksummed release instead")
	}
	if !regexp.MustCompile(`sha256sum -c`).MatchString(src) {
		t.Error("the flyctl download must be verified against a pinned SHA-256")
	}
	if n := strings.Count(src, "flyctl deploy") + strings.Count(src, "fly deploy"); n != 1 {
		t.Errorf("expected exactly one deploy command, found %d", n)
	}
	if !strings.Contains(src, "--app hive-relay-enterprise --config deploy/fly.toml") {
		t.Error("deploy must target hive-relay-enterprise with deploy/fly.toml")
	}
	for _, gate := range []string{"TOKEN_PUBKEY", "ACCESS_TOKENS", "ENTERPRISE", "--dockerfile", "--image", "--build-arg"} {
		if strings.Contains(src, gate) {
			t.Errorf("deploy.yml must not set %s: the public relay is the open binary from the root Dockerfile", gate)
		}
	}

	fly := readRepoFile(t, "deploy/fly.toml")
	if !strings.Contains(fly, `dockerfile = "../Dockerfile"`) {
		t.Error("deploy/fly.toml must build the repo-root Dockerfile")
	}
	for _, gate := range []string{"TOKEN_PUBKEY", "ACCESS_TOKENS"} {
		if strings.Contains(fly, gate) {
			t.Errorf("deploy/fly.toml must not set %s", gate)
		}
	}
	df := readRepoFile(t, "Dockerfile")
	if !strings.Contains(df, "go build -trimpath -ldflags=\"-s -w\" -o /out/langstroth-relay ./cmd/langstroth-relay") {
		t.Error("root Dockerfile must build the open cmd/langstroth-relay binary")
	}
}
