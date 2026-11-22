package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// `langstroth-relay check --json` prints a ligus.checks.v1 report, and the
// deploy/agent scripts' reports share its core keys. These tests read the
// scripts' contract out of the scripts, run one of them, and require check's
// JSON to carry every key the scripts do, plus the ligus.checks.v1 ones.

// repoRoot is the directory holding go.mod (the package may move under it).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the package")
		}
		dir = parent
	}
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keySet(ks []string) string { sort.Strings(ks); return strings.Join(ks, ",") }

// scriptContract reads the keys every deploy/agent report with checks
// carries (the intersection over the scripts' printf formats) and the keys
// of one check (lib.sh's check function).
func scriptContract(t *testing.T) (top, item []string) {
	t.Helper()
	agent := filepath.Join(repoRoot(t), "deploy", "agent")
	scripts, _ := filepath.Glob(filepath.Join(agent, "*.sh"))
	if len(scripts) == 0 {
		t.Fatalf("no scripts in %s", agent)
	}
	format := regexp.MustCompile(`printf '(\{"tool":[^']*)'`)
	key := regexp.MustCompile(`"([A-Za-z_]+)":`)
	count := map[string]int{}
	reports := 0
	for _, f := range scripts {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range format.FindAllStringSubmatch(string(src), -1) {
			if !strings.Contains(m[1], `"checks":`) {
				continue // bootstrap-secrets reports secrets, not checks
			}
			reports++
			seen := map[string]bool{}
			for _, k := range key.FindAllStringSubmatch(m[1], -1) {
				if !seen[k[1]] {
					seen[k[1]] = true
					count[k[1]]++
				}
			}
		}
	}
	if reports < 5 {
		t.Fatalf("found %d script reports with checks, want at least 5 (did the printf formats change?)", reports)
	}
	for k, n := range count {
		if n == reports {
			top = append(top, k)
		}
	}

	lib, err := os.ReadFile(filepath.Join(agent, "lib.sh"))
	if err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`CHECKS\+=\("(.*)"\)`).FindStringSubmatch(string(lib))
	if line == nil {
		t.Fatal(`lib.sh: no CHECKS+=("...") line`)
	}
	for _, k := range regexp.MustCompile(`\\"([A-Za-z_]+)\\":`).FindAllStringSubmatch(line[1], -1) {
		item = append(item, k[1])
	}
	return top, item
}

// decodeReport splits a report into its top-level keys and its checks.
func decodeReport(t *testing.T, out []byte) (top map[string]json.RawMessage, checks []map[string]json.RawMessage) {
	t.Helper()
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("not one JSON object: %v\n%s", err, out)
	}
	if err := json.Unmarshal(top["checks"], &checks); err != nil {
		t.Fatalf("checks: %v\n%s", err, out)
	}
	return top, checks
}

func TestCheckJSONMatchesDeployAgent(t *testing.T) {
	top, item := scriptContract(t)
	if got, want := keySet(top), "checks,ok,tool"; got != want {
		t.Fatalf("deploy/agent report keys = %s, want %s (update check.go and its help with the scripts)", got, want)
	}
	if got, want := keySet(item), "detail,fix,id,name,status"; got != want {
		t.Fatalf("deploy/agent check keys = %s, want %s (update check.go and its help with the scripts)", got, want)
	}
	// check's own keys: ligus.checks.v1, plus name for one release.
	const wantTop = "checks,command,exitCode,next,ok,schema,tool"
	const wantItem = "detail,fix,id,name,status,title"
	for _, k := range top {
		if !strings.Contains(","+wantTop+",", ","+k+",") {
			t.Fatalf("the scripts' report key %q is missing from check --json", k)
		}
	}
	for _, k := range item {
		if !strings.Contains(","+wantItem+",", ","+k+",") {
			t.Fatalf("the scripts' check key %q is missing from check --json", k)
		}
	}

	// A real script report: local.sh status on an empty data dir fails one
	// check (exit 1, ok false) without a cluster or a relay.
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	home := t.TempDir()
	cmd := exec.Command(bash, filepath.Join(repoRoot(t), "deploy", "agent", "local.sh"),
		"status", "--data-dir", filepath.Join(home, "data"), "--json")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("local.sh status: %v, want exit 1\n%s%s", err, out, stderr.String())
	}
	sTop, sChecks := decodeReport(t, out)
	for _, k := range top {
		if _, ok := sTop[k]; !ok {
			t.Errorf("local.sh report lacks %q: %s", k, out)
		}
	}
	if string(sTop["ok"]) != "false" || len(sChecks) == 0 {
		t.Fatalf("local.sh report: %s", out)
	}
	scriptItem := keySet(sortedKeys(sChecks[0]))
	if scriptItem != keySet(item) {
		t.Fatalf("local.sh check keys %s, lib.sh says %s", scriptItem, keySet(item))
	}

	// check --json, failing (exit 1 like the script) and passing: the same
	// report keys (the scripts' context keys aside) and the same check keys,
	// on every check, fix included when it is empty.
	for _, failing := range []bool{true, false} {
		clearSettings(t)
		t.Setenv("LANGSTROTH_RELAY_DATA_DIR", t.TempDir())
		if failing {
			t.Setenv("LANGSTROTH_RELAY_REQUIRE_DB", "1")
		}
		var b bytes.Buffer
		err := cmdCheck([]string{"--json"}, &b)
		if failing != errors.Is(err, errChecksFailed) {
			t.Fatalf("failing=%v: cmdCheck returned %v", failing, err)
		}
		if n := strings.Count(b.String(), "\n"); n != 1 || !strings.HasSuffix(b.String(), "\n") {
			t.Errorf("check --json printed %d lines, want one object on one line", n)
		}
		rTop, rChecks := decodeReport(t, b.Bytes())
		if got := keySet(sortedKeys(rTop)); got != wantTop {
			t.Errorf("check --json keys %s, want %s", got, wantTop)
		}
		if string(rTop["schema"]) != `"ligus.checks.v1"` || string(rTop["tool"]) != `"langstroth-relay"` || string(rTop["command"]) != `"check"` {
			t.Errorf("schema, tool, command: %s %s %s", rTop["schema"], rTop["tool"], rTop["command"])
		}
		wantExit := "0"
		if failing {
			wantExit = "1"
		}
		if string(rTop["exitCode"]) != wantExit {
			t.Errorf("failing=%v: exitCode %s, want %s", failing, rTop["exitCode"], wantExit)
		}
		var next []string
		if err := json.Unmarshal(rTop["next"], &next); err != nil || (failing && len(next) == 0) {
			t.Errorf("failing=%v: next %s (a failed run names the next step)", failing, rTop["next"])
		}
		wantOK := "true"
		if failing {
			wantOK = "false"
		}
		if string(rTop["ok"]) != wantOK {
			t.Errorf("failing=%v: ok is %s, want %s", failing, rTop["ok"], wantOK)
		}
		for _, c := range rChecks {
			if got := keySet(sortedKeys(c)); got != wantItem {
				t.Errorf("check %s keys %s, want %s", c["id"], got, wantItem)
			}
			if string(c["id"]) != string(c["name"]) {
				t.Errorf("check id %s and name %s differ", c["id"], c["name"])
			}
			var st string
			_ = json.Unmarshal(c["status"], &st)
			if !strings.Contains(",ok,warn,fail,skip,", ","+st+",") {
				t.Errorf("check %s status %q is not ok, warn, fail or skip", c["id"], st)
			}
		}
	}
}

// The text form: a header, one line per check, a fix line under anything
// that isn't ok, and the summary with the next step.
func TestCheckTextForm(t *testing.T) {
	clearSettings(t)
	t.Setenv("LANGSTROTH_RELAY_DATA_DIR", t.TempDir())
	t.Setenv("LANGSTROTH_RELAY_REQUIRE_DB", "1")
	var b bytes.Buffer
	if err := cmdCheck(nil, &b); !errors.Is(err, errChecksFailed) {
		t.Fatalf("cmdCheck: %v", err)
	}
	out := b.String()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if lines[0] != "langstroth-relay check: this environment's settings" {
		t.Errorf("header %q", lines[0])
	}
	if !strings.Contains(out, "\nfail  store             LANGSTROTH_RELAY_REQUIRE_DB is set") ||
		!strings.Contains(out, "\n      fix: set DATABASE_URL") {
		t.Errorf("the failed store check and its fix:\n%s", out)
	}
	if !strings.Contains(out, "\nok    old names ") {
		t.Errorf("a passed check says ok:\n%s", out)
	}
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "1 failure, 2 warnings · next: langstroth-relay help troubleshooting") {
		t.Errorf("summary %q", last)
	}
}
