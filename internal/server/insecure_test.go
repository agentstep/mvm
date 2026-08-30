package server

import "testing"

// MVM_INSECURE disables TLS on the TCP listener, putting the API bearer token — root over every VM
// on the host — on the wire in cleartext. Defensible against 127.0.0.1 while debugging; indefensible
// on any address another machine can reach. The env var alone could not tell those apart.
func TestLoopbackDetectionForInsecureListener(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:19876", true},
		{"localhost:19876", true},
		{"[::1]:19876", true},
		{"127.0.0.1", true},

		// The dangerous shapes. ":19876" is the one most likely to be typed by accident, and it
		// binds every interface.
		{":19876", false},
		{"0.0.0.0:19876", false},
		{"", false},
		{"192.168.1.10:19876", false},
		{"10.0.0.5:19876", false},
		{"[::]:19876", false},
		{"mvm.example.com:19876", false},
	} {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v — a wrong answer here either blocks a legitimate debug session or serves a root token in the clear", tc.addr, got, tc.want)
		}
	}
}
