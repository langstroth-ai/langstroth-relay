package relay

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// renderChart runs helm template on the chart; skipped when helm isn't
// installed (CI's kind-e2e job has it).
func renderChart(t *testing.T, args ...string) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed")
	}
	// Read every chart file so go test's cache notices chart edits (it can't
	// see the files helm opens).
	_ = filepath.WalkDir("deploy/helm/ligus-relay", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			_, _ = os.ReadFile(p)
		}
		return nil
	})
	cmd := exec.Command(helm, append([]string{"template", "t", "deploy/helm/ligus-relay", "--kube-version", "1.30.0"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// docs splits rendered YAML into documents.
func docs(out string) []string { return strings.Split(out, "\n---\n") }

func docOfKind(out, kind, nameSuffix string) string {
	for _, d := range docs(out) {
		if strings.Contains(d, "kind: "+kind+"\n") && strings.Contains(d, "name: t-ligus-relay"+nameSuffix+"\n") {
			return d
		}
	}
	return ""
}

// H-01: the chart defaulted tunnel.mode to open, so a default install was a
// public tunnel provider: any host on the internet could register (a durable
// store row per key pair) and relay phone sessions, although the binary's own
// default is off.
func TestChartTunnelOffByDefault(t *testing.T) {
	out := renderChart(t)
	if !regexp.MustCompile(`name: LIGUS_RELAY_TUNNEL\s+value: "off"`).MatchString(out) {
		t.Fatalf("default install must render LIGUS_RELAY_TUNNEL=off")
	}
	split := renderChart(t, "--set", "postgres.enabled=true")
	if docOfKind(split, "Deployment", "-tunnel") != "" {
		t.Fatal("default split install must not render a tunnel Deployment")
	}
	if !strings.Contains(renderChart(t, "--set", "tunnel.mode=open"), "value: \"open\"") {
		t.Fatal("tunnel.mode=open must still render")
	}
}
