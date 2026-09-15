package server

import "testing"

func TestDefaultAddrIsLoopback(t *testing.T) {
	if !IsLoopback(DefaultAddr) {
		t.Fatalf("DefaultAddr %q is not loopback -- this server exposes a team's whole "+
			"alerting posture and must not default to a wider bind", DefaultAddr)
	}
}

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:9091", true},
		{"127.0.0.1", true},
		{"localhost:9091", true},
		{"localhost", true},
		{"::1", true},
		{"[::1]:9091", true},
		{"0.0.0.0:9091", false},
		{"0.0.0.0", false},
		{"192.168.1.5:9091", false},
		{"example.com:9091", false},
		{":9091", false},
	}
	for _, c := range cases {
		if got := IsLoopback(c.addr); got != c.want {
			t.Errorf("IsLoopback(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}
