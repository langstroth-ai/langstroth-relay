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
	split := renderChart(t, "--set", "postgres.enabled=true", "--set", "postgres.existingSecret=pg")
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
	out := renderChart(t, "--set", "postgres.enabled=true", "--set", "postgres.existingSecret=pg", "--set", "tunnel.mode=open")
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

// Regression: the APNs key reaches the tunnel pod only, never relay-sync.
func TestChartAPNsKeyOnlyOnTunnelPod(t *testing.T) {
	out := renderChart(t, "--set", "postgres.enabled=true", "--set", "postgres.existingSecret=pg", "--set", "tunnel.mode=open",
		"--set", "tunnel.apns.existingSecret=apns", "--set", "tunnel.apns.keyId=K", "--set", "tunnel.apns.teamId=T", "--set", "tunnel.apns.topic=x.y")
	if d := docOfKind(out, "Deployment", "-sync"); strings.Contains(d, "APNS") || strings.Contains(d, "secretName: apns") {
		t.Fatal("relay-sync must not get the APNs key")
	}
	if d := docOfKind(out, "Deployment", "-tunnel"); !strings.Contains(d, "LIGUS_RELAY_APNS_KEY_FILE") || !strings.Contains(d, "secretName: apns") {
		t.Fatal("the tunnel pod should get the APNs key file")
	}
}

// renderChartErr runs helm template and returns its error output (or "" when
// it rendered).
func renderChartErr(t *testing.T, args ...string) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed")
	}
	cmd := exec.Command(helm, append([]string{"template", "t", "deploy/helm/ligus-relay", "--kube-version", "1.30.0"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return ""
	}
	return string(out)
}

// The APNs key must come only from the tunnel-only Secret: config.existingSecret
// and config.env reach every pod, relay-sync included.
func TestChartAPNsKeyNotThroughSharedConfig(t *testing.T) {
	base := []string{"--set", "postgres.enabled=true", "--set", "postgres.existingSecret=pg", "--set", "tunnel.mode=open"}
	if msg := renderChartErr(t, append(base, "--set", "config.existingSecret=shared", "--set", "tunnel.apns.existingSecret=shared")...); !strings.Contains(msg, "tunnel.apns.existingSecret") {
		t.Fatalf("sharing config.existingSecret with the APNs Secret must fail, got %q", msg)
	}
	for _, k := range []string{"LIGUS_RELAY_APNS_KEY", "HIVE_RELAY_APNS_KEY_FILE", "LIGUS_RELAY_APNS_TOPIC"} {
		if msg := renderChartErr(t, append(base, "--set", "config.env."+k+"=x")...); !strings.Contains(msg, "APNs") {
			t.Fatalf("config.env.%s must fail, got %q", k, msg)
		}
	}
	if msg := renderChartErr(t, append(base, "--set", "config.existingSecret=shared", "--set", "tunnel.apns.existingSecret=apns")...); msg != "" {
		t.Fatalf("separate Secrets must render: %s", msg)
	}
}

// Egress DNS was open to port 53 anywhere, so a compromised relay pod could
// tunnel data out over DNS to any resolver. It is limited to kube-dns.
func TestChartDNSEgressOnlyToKubeDNS(t *testing.T) {
	np := docOfKind(renderChart(t), "NetworkPolicy", "")
	i := strings.Index(np, "egress:")
	if i < 0 {
		t.Fatal("no egress rules")
	}
	egress := np[i:]
	j := strings.Index(egress, "port: 53")
	if j < 0 {
		t.Fatal("no DNS egress rule")
	}
	rule := egress[:j]
	for _, want := range []string{"namespaceSelector:", "kubernetes.io/metadata.name: kube-system", "podSelector:", "k8s-app: kube-dns"} {
		if !strings.Contains(rule, want) {
			t.Fatalf("DNS egress rule lacks %q:\n%s", want, egress)
		}
	}
	custom := docOfKind(renderChart(t, "--set", "networkPolicy.dns.podSelector.matchLabels.k8s-app=coredns"), "NetworkPolicy", "")
	if !strings.Contains(custom, "k8s-app: coredns") {
		t.Fatal("networkPolicy.dns.podSelector must be overridable")
	}
}

// v0.3: contacts on and the deprecated GitHub routes off by default; the
// contacts values render as their LIGUS_RELAY_* variables, integers intact.
func TestChartContactsAndGitHubRoutes(t *testing.T) {
	out := renderChart(t)
	cm := docOfKind(out, "ConfigMap", "")
	if strings.Contains(cm, "LIGUS_RELAY_CONTACT") || strings.Contains(cm, "LIGUS_RELAY_GITHUB_ROUTES") {
		t.Fatalf("default install must keep the binary defaults:\n%s", cm)
	}
	out = renderChart(t, "--set", "contacts.enabled=false", "--set", "contacts.dropMaxBoxBytes=4194304",
		"--set", "contacts.dropRate=0.5", "--set", "githubRoutes=true")
	cm = docOfKind(out, "ConfigMap", "")
	for _, want := range []string{
		`LIGUS_RELAY_CONTACTS: "off"`,
		`LIGUS_RELAY_CONTACT_DROP_MAX_BOX_BYTES: "4194304"`,
		`LIGUS_RELAY_CONTACT_DROP_RATE: "0.5"`,
		`LIGUS_RELAY_GITHUB_ROUTES: "on"`,
	} {
		if !strings.Contains(cm, want) {
			t.Errorf("missing %s in:\n%s", want, cm)
		}
	}
}

// Secrets are files, never environment variables, and the chart generates
// none (lookup-generated passwords changed on every helm template / GitOps
// render).
func TestChartSecretsAreFiles(t *testing.T) {
	out := renderChart(t, "--set", "postgres.enabled=true", "--set", "postgres.existingSecret=pg",
		"--set", "config.existingSecret=relay-env", "--set", "tunnel.mode=open")
	if strings.Contains(out, "secretKeyRef") || strings.Contains(out, "secretRef:") {
		t.Fatal("a Secret reaches a container through env or envFrom")
	}
	for _, want := range []string{"DATABASE_URL_FILE", "LIGUS_RELAY_SECRETS_DIR", "POSTGRES_PASSWORD_FILE", "checksum/secrets:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	if regexp.MustCompile(`(?m)^kind: Secret$`).MatchString(out) {
		t.Fatal("the in-chart Postgres must not render (generate) a Secret")
	}
	if strings.Contains(out, "defaultMode: 0440") || !strings.Contains(out, "defaultMode: 0400") {
		t.Fatal("secret volumes must be 0400")
	}
	again := renderChart(t, "--set", "postgres.enabled=true", "--set", "postgres.existingSecret=pg",
		"--set", "config.existingSecret=relay-env", "--set", "tunnel.mode=open")
	if again != out {
		t.Fatal("two renders differ")
	}
	if err := exec.Command("helm", "template", "t", "deploy/helm/ligus-relay", "--kube-version", "1.30.0",
		"--set", "postgres.enabled=true").Run(); err == nil {
		t.Fatal("postgres.enabled without postgres.existingSecret must fail")
	}
}
