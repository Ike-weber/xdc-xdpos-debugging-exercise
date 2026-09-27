// Copyright 2026 XDC Network
// Tests for the XDC default-on ethstats URL: xdcnode.sh-style node name
// (host-network-nodetype-os-location) + public IP, default-on without --ethstats.

package utils

import (
	"strings"
	"testing"
)

// parseLikeEthstats mirrors ethstats.parseEthstatsURL exactly (LastIndex '@',
// then LastIndex ':' in the pre-host part) so we can prove URLs produced here are
// accepted and split the way the real stats client expects.
func parseLikeEthstats(url string) (name, pass, host string, ok bool) {
	at := strings.LastIndex(url, "@")
	if at == -1 || at == len(url)-1 {
		return "", "", "", false
	}
	preHost := url[:at]
	host = url[at+1:]
	c := strings.LastIndex(preHost, ":")
	if c == -1 {
		return preHost, "", host, true
	}
	name = preHost[:c]
	if c != len(preHost)-1 {
		pass = preHost[c+1:]
	}
	return name, pass, host, true
}

// XDCEthstatsURL("", …) does a network call (ipinfo.io) for public IP+country.
// Here we only assert the structure that is independent of that lookup: it must
// parse, carry the board secret+host, and the name must follow the
// host-network-nodetype-os convention.
func TestXDCEthstatsURL_SynthesizeStructure(t *testing.T) {
	got := XDCEthstatsURL("", 51, "fast") // apothem, fast
	name, pass, host, ok := parseLikeEthstats(got)
	if !ok {
		t.Fatalf("synthesized URL not parseable by the stats client: %q", got)
	}
	if pass != xdcDefaultStatsSecret {
		t.Errorf("secret = %q, want %q (url=%q)", pass, xdcDefaultStatsSecret, got)
	}
	if host != xdcDefaultStatsHost {
		t.Errorf("host = %q, want %q (url=%q)", host, xdcDefaultStatsHost, got)
	}
	// name = host-apothem-fast-os-location[-ip]; assert the fixed middle parts.
	if !strings.Contains(name, "-apothem-fast-") {
		t.Errorf("name %q missing \"-apothem-fast-\" segment (url=%q)", name, got)
	}
	parts := strings.Split(name, "-")
	if len(parts) < 5 {
		t.Errorf("name %q has %d dash-parts, want >=5 (host-network-nodetype-os-location)", name, len(parts))
	}
	t.Logf("apothem/fast, no --ethstats  ->  %s", got)
}

func TestXdcAppendIPToName(t *testing.T) {
	const ip = "1.2.3.4"
	cases := []struct{ name, in, want string }{
		{"with secret", "mynode:sek@h:443", "mynode-1.2.3.4:sek@h:443"},
		{"no secret", "mynode@h:443", "mynode-1.2.3.4@h:443"},
		{"already has ip (idempotent)", "mynode-1.2.3.4:sek@h:443", "mynode-1.2.3.4:sek@h:443"},
		{"empty url", "", ""},
		{"no at-sign", "garbage", "garbage"},
	}
	for _, c := range cases {
		if got := xdcAppendIPToName(c.in, ip); got != c.want {
			t.Errorf("%s: xdcAppendIPToName(%q,%q) = %q, want %q", c.name, c.in, ip, got, c.want)
		}
	}
	// empty ip is a no-op
	if got := xdcAppendIPToName("mynode:sek@h:443", ""); got != "mynode:sek@h:443" {
		t.Errorf("empty ip must be a no-op, got %q", got)
	}
}

func TestXdcSanitizeAZ(t *testing.T) {
	for in, want := range map[string]string{"US": "us", "in": "in", "U.S.A": "usa", "123": "", "Fr-2": "fr"} {
		if got := xdcSanitizeAZ(in); got != want {
			t.Errorf("xdcSanitizeAZ(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestXDCNetworkLabel(t *testing.T) {
	for id, want := range map[uint64]string{50: "mainnet", 51: "apothem", 551: "devnet", 999: "chain999"} {
		if got := xdcNetworkLabel(id); got != want {
			t.Errorf("xdcNetworkLabel(%d) = %q, want %q", id, got, want)
		}
	}
}

func TestXdcHostLabel(t *testing.T) {
	// Just assert it is non-empty and sanitised (lowercase alphanumeric only).
	h := xdcHostLabel()
	if h == "" {
		t.Fatal("xdcHostLabel returned empty")
	}
	for _, r := range h {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			t.Errorf("xdcHostLabel %q contains non-alphanumeric %q", h, r)
		}
	}
	t.Logf("host label on this machine: %s", h)
}
