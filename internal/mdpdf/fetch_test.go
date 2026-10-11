package mdpdf

import (
	"net/netip"
	"testing"
)

func TestIsPublic(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1",
		"0.0.0.0", "::1", "fe80::1", "fc00::1", "::ffff:127.0.0.1", "64:ff9b::a9fe:a9fe", "224.0.0.1"}
	for _, s := range blocked {
		if isPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "142.250.1.1", "2606:4700:4700::1111"} {
		if !isPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}
