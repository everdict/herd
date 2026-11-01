package registry

import "testing"

// TestPlainHosts (criterion insecure-registry): PlainHosts speaks plain HTTP to the loopback hosts and to exactly the
// named host[:port]s, as the docker daemon's insecure-registries; every other host, the same address on another port
// or without one included, stays https.
func TestPlainHosts(t *testing.T) {
	plain := PlainHosts([]string{"100.69.164.81:5001", "Registry.Lab"})
	for host, want := range map[string]bool{
		"100.69.164.81:5001": true,
		"registry.lab":       true,
		"REGISTRY.LAB":       true,
		"localhost:5000":     true,
		"127.0.0.1:5000":     true,
		"100.69.164.81":      false,
		"100.69.164.81:5002": false,
		"100.69.164.82:5001": false,
		"registry.lab:5000":  false,
		"ghcr.io":            false,
	} {
		if got := plain(host); got != want {
			t.Errorf("plain(%q) = %v, want %v", host, got, want)
		}
	}
	var c Client
	if got := c.base("100.69.164.81:5001"); got != "https://100.69.164.81:5001" {
		t.Errorf("default client: %s, want https for a non-loopback host", got)
	}
	c.PlainHTTP = plain
	if got := c.base("100.69.164.81:5001"); got != "http://100.69.164.81:5001" {
		t.Errorf("with PlainHosts: %s, want plain HTTP to the named host", got)
	}
}
