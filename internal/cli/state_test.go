package cli

import (
	"testing"
)

func TestIsPrivilegedPort(t *testing.T) {
	cases := []struct {
		port int
		want bool
	}{
		{0, false},
		{80, true},
		{443, true},
		{1023, true},
		{1024, false},
		{8443, false},
		{-1, false},
	}
	for _, c := range cases {
		got := isPrivilegedPort(c.port)
		if got != c.want {
			t.Errorf("isPrivilegedPort(%d) = %v, want %v", c.port, got, c.want)
		}
	}
}

func TestPortFromAddr(t *testing.T) {
	cases := []struct {
		addr string
		want int
	}{
		{":443", 443},
		{"127.0.0.1:8443", 8443},
		{":0", 0},
		{"localhost", 0},
		{"", 0},
	}
	for _, c := range cases {
		got := portFromAddr(c.addr)
		if got != c.want {
			t.Errorf("portFromAddr(%q) = %d, want %d", c.addr, got, c.want)
		}
	}
}

func TestSudoReexecCmd(t *testing.T) {
	cmd, err := sudoReexecCmd("proxy", "start", "--port", "443")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Args[0] != "sudo" {
		t.Errorf("first arg = %q, want sudo", cmd.Args[0])
	}
	// The second arg should be the executable path.
	if cmd.Args[1] == "" {
		t.Error("executable path is empty")
	}
	// Remaining args should be the passthrough.
	wantTail := []string{"proxy", "start", "--port", "443"}
	gotTail := cmd.Args[2:]
	if len(gotTail) != len(wantTail) {
		t.Fatalf("tail args = %v, want %v", gotTail, wantTail)
	}
	for i := range wantTail {
		if gotTail[i] != wantTail[i] {
			t.Errorf("tail[%d] = %q, want %q", i, gotTail[i], wantTail[i])
		}
	}
}
