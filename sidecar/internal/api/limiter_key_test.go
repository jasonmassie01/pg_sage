package api

import "testing"

// No concurrent access test: ClientLimiterKey is a pure function.

func TestClientLimiterKey_IPv6SharesSlash64(t *testing.T) {
	a, b := ClientLimiterKey("2001:db8::1"), ClientLimiterKey("2001:db8::2")
	if a != b {
		t.Fatalf("2001:db8::1 -> %q and 2001:db8::2 -> %q, want one /64 bucket", a, b)
	}
	if a != "2001:db8::/64" {
		t.Fatalf("key = %q, want 2001:db8::/64", a)
	}
	if got := ClientLimiterKey("2001:db8:0:1::1"); got == a {
		t.Fatalf("2001:db8:0:1::1 shares %q with another /64", got)
	}
	if got := ClientLimiterKey("2001:DB8::ffff:1"); got != a {
		t.Fatalf("upper-case spelling -> %q, want %q", got, a)
	}
}

func TestClientLimiterKey_IPv4Unchanged(t *testing.T) {
	cases := map[string]string{
		"10.0.0.1":         "10.0.0.1",
		"10.0.0.2":         "10.0.0.2",
		"::ffff:10.0.0.1":  "10.0.0.1",
		"203.0.113.255":    "203.0.113.255",
		"0.0.0.0":          "0.0.0.0",
		"255.255.255.255":  "255.255.255.255",
		"::ffff:192.0.2.9": "192.0.2.9",
	}
	for in, want := range cases {
		if got := ClientLimiterKey(in); got != want {
			t.Errorf("ClientLimiterKey(%q) = %q, want %q", in, got, want)
		}
	}
	if ClientLimiterKey("10.0.0.1") == ClientLimiterKey("10.0.0.2") {
		t.Fatal("two IPv4 clients share a bucket")
	}
}

func TestClientLimiterKey_NonAddressPassesThrough(t *testing.T) {
	for _, in := range []string{"", "not-an-ip", "databricks-app:abc123",
		"10.0.0.1:443", "[2001:db8::1]"} {
		if got := ClientLimiterKey(in); got != in {
			t.Errorf("ClientLimiterKey(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestClientLimiterKey_Slash64Boundary(t *testing.T) {
	low := ClientLimiterKey("2001:db8:1:2::")
	high := ClientLimiterKey("2001:db8:1:2:ffff:ffff:ffff:ffff")
	next := ClientLimiterKey("2001:db8:1:3::")
	if low != high {
		t.Fatalf("first and last address of one /64 -> %q and %q", low, high)
	}
	if low == next {
		t.Fatalf("adjacent /64s share key %q", low)
	}
}
