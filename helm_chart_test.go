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

// H-02: the liveness probe hit /v1/health, which round-trips the database, so
// a database outage or a slow query made the kubelet restart every relay pod
// (dropping every tunnel socket) without fixing anything. Liveness must only
// check the process; readiness keeps the store check.
func TestChartLivenessDoesNotDependOnStore(t *testing.T) {
	out := renderChart(t, "--set", "postgres.enabled=true", "--set", "tunnel.mode=open")
	for _, suffix := range []string{"-sync", "-tunnel"} {
		d := docOfKind(out, "Deployment", suffix)
		if d == "" {
			t.Fatalf("no %s Deployment", suffix)
		}
		i := strings.Index(d, "livenessProbe:")
		if i < 0 {
			t.Fatalf("%s: no livenessProbe", suffix)
		}
		probe := d[i:]
		if j := strings.Index(probe, "resources:"); j > 0 {
			probe = probe[:j]
		}
		if strings.Contains(probe, "/v1/health") {
			t.Errorf("%s: livenessProbe depends on the store via /v1/health:\n%s", suffix, probe)
		}
		if !strings.Contains(d, "readinessProbe:\n") || !strings.Contains(d, "path: /v1/health") {
			t.Errorf("%s: readinessProbe should still check /v1/health", suffix)
		}
	}
}
