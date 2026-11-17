package relay

import "testing"

// Adversarial review of database.mode (k8s/postgres).

// R-1: the external-mode CIDR checks only looked at IPv4 text. IPv6 had the
// same /8 floor as IPv4 (an IPv6 /8 is 2^120 addresses), and an IPv4-mapped
// or IPv6 metadata address was accepted.
func TestChartDatabaseExternalCIDRRefusesIPv6Bypasses(t *testing.T) {
	for _, cidr := range []string{
		"::/0",
		"::/8",
		"2001:db8::/16",
		"::ffff:169.254.169.254/128",
		"::ffff:a9fe:a9fe/128",
		"::ffff:0:0/96",
		"0:0:0:0:0:ffff:a9fe:a9fe/128",
		"::169.254.169.254/128",
		"64:ff9b::a9fe:a9fe/128",
		"fd00:ec2::254/128",
		"FD00:0EC2:0000::/48",
	} {
		msg := renderChartErr(t, "--set", "database.mode=external", "--set", "database.external.existingSecret=db",
			"--set", "database.external.cidrs[0]="+cidr)
		if msg == "" {
			t.Errorf("cidr %s rendered; want a refusal", cidr)
		}
	}
	for _, ok := range []string{"2001:db8:1:2::15/128", "2001:db8:1::/48", "10.20.0.15/32"} {
		if msg := renderChartErr(t, "--set", "database.mode=external", "--set", "database.external.existingSecret=db",
			"--set", "database.external.cidrs[0]="+ok); msg != "" {
			t.Errorf("cidr %s must render: %s", ok, msg)
		}
	}
}
