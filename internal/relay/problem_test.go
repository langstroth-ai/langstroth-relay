package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Errors share one shape: "langstroth-relay: <what>", an indented fix line,
// and with --json one ligus.error.v1 document on stdout.
func TestReportErrorShape(t *testing.T) {
	var se, so bytes.Buffer
	code := reportError(&se, &so, usageErrorf("check: unknown flag --nope"), "check", false)
	if code != exitUsage {
		t.Fatalf("exit %d", code)
	}
	if got, want := se.String(), "langstroth-relay: check: unknown flag --nope\n  fix: see langstroth-relay check --help\n"; got != want {
		t.Errorf("stderr %q, want %q", got, want)
	}
	if so.Len() != 0 {
		t.Errorf("no JSON without --json: %q", so.String())
	}

	se.Reset()
	code = reportError(&se, &so, &problem{Code: "relay_unreachable", Message: "no answer", Why: "connection refused", Fix: "start it", exit: 7}, "check", true)
	if code != 7 || !strings.Contains(se.String(), "\n  why: connection refused\n  fix: start it\n") {
		t.Errorf("exit %d, stderr %q", code, se.String())
	}
	var doc struct {
		Schema, Tool, Command string
		ExitCode              int
		Error                 map[string]string
	}
	if err := json.Unmarshal(so.Bytes(), &doc); err != nil {
		t.Fatalf("stdout %q: %v", so.String(), err)
	}
	if doc.Schema != "ligus.error.v1" || doc.Tool != "langstroth-relay" || doc.Command != "check" || doc.ExitCode != 7 ||
		doc.Error["code"] != "relay_unreachable" || doc.Error["why"] != "connection refused" || doc.Error["fix"] != "start it" {
		t.Errorf("document %+v", doc)
	}

	// A failed check printed its own report: nothing more, exit 1.
	se.Reset()
	so.Reset()
	if code := reportError(&se, &so, errChecksFailed, "check", true); code != exitFailed || se.Len()+so.Len() != 0 {
		t.Errorf("checks failed: exit %d, %q %q", code, se.String(), so.String())
	}
	if code := reportError(&se, &so, errors.New("boom"), "issue", false); code != exitFailed || !strings.HasPrefix(se.String(), "langstroth-relay: boom\n") {
		t.Errorf("plain error: exit %d %q", code, se.String())
	}
}

func TestWantsJSON(t *testing.T) {
	for args, want := range map[string]bool{"--json": true, "-o json": true, "--output=json": true, "-o text": false, "-- --json": false, "": false} {
		if got := wantsJSON(strings.Fields(args)); got != want {
			t.Errorf("wantsJSON(%q) = %v", args, got)
		}
	}
}

// Every command's help names its exit codes and the shared table.
func TestEveryHelpHasExitCodes(t *testing.T) {
	for _, name := range []string{"check", "keygen", "issue", "bootstrap-agent", "exit-codes"} {
		h := helpText(t, name)
		if !strings.Contains(h, "Exit codes") && !strings.Contains(h, "EXIT CODES") {
			t.Errorf("help %s has no exit codes", name)
		}
	}
	var b bytes.Buffer
	_ = cmdHelp(nil, &b)
	for _, want := range []string{"5  not found", "7  offline", "ligus.error.v1"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("top-level help lacks %q", want)
		}
	}
}

func TestSplitFlagValues(t *testing.T) {
	got := strings.Join(splitFlagValues([]string{"--url=https://r.example/x=1", "--json", "-o=json", "--", "--a=b"}), " ")
	if want := "--url https://r.example/x=1 --json -o=json -- --a=b"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCheckOutputFlag(t *testing.T) {
	clearSettings(t)
	t.Setenv("LANGSTROTH_RELAY_DATA_DIR", t.TempDir())
	var b bytes.Buffer
	if err := cmdCheck([]string{"-o", "json"}, &b); err != nil || !strings.HasPrefix(b.String(), `{"schema":"ligus.checks.v1"`) {
		t.Errorf("-o json: %v %q", err, b.String())
	}
	b.Reset()
	if err := cmdCheck([]string{"--output=text"}, &b); err != nil || !strings.HasPrefix(b.String(), "langstroth-relay check:") {
		t.Errorf("--output=text: %v %q", err, b.String())
	}
	if err := cmdCheck([]string{"-o", "yaml"}, &b); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("-o yaml: %v", err)
	}
}
