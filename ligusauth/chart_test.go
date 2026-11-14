package ligusauth

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// The Helm chart renders a config.json that the binary's strict decoder
// accepts (no unknown or misspelled fields).
func TestHelmChartRendersAValidConfig(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed")
	}
	chart := "../deploy/helm/ligus-auth"
	out, err := exec.Command(helm, "template", "t", chart, "-f", chart+"/ci/dex-values.yaml", "--kube-version", "1.30.0", "--show-only", "templates/configmap.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	_, after, ok := strings.Cut(string(out), "config.json: |\n")
	if !ok {
		t.Fatal("no config.json in the ConfigMap")
	}
	var lines []string
	for _, l := range strings.Split(after, "\n") {
		if !strings.HasPrefix(l, "    ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(l, "    "))
	}
	dec := json.NewDecoder(strings.NewReader(strings.Join(lines, "\n")))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("rendered config does not decode strictly: %v", err)
	}
	if c.SigningKeyFile == "" || c.Store.Driver != "file" || len(c.Tenants) != 1 || c.Tenants[0].OIDC.Issuer == "" {
		t.Fatalf("rendered config: %+v", c)
	}
}
