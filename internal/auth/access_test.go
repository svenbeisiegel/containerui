package auth

import "testing"

func TestSocketAllows(t *testing.T) {
	docker := SocketOwner{UID: 0, GID: 998, Mode: 0o660} // root:docker srw-rw----
	cases := []struct {
		name string
		uid  string
		gids []string
		s    SocketOwner
		want bool
	}{
		{"root", "0", nil, docker, true},
		{"docker group member", "1000", []string{"1000", "998"}, docker, true},
		{"not in group", "1000", []string{"1000", "27"}, docker, false},
		{"group without write", "1000", []string{"998"}, SocketOwner{GID: 998, Mode: 0o640}, false},
		{"owner", "1000", nil, SocketOwner{UID: 1000, GID: 1000, Mode: 0o600}, true},
		{"owner without perms", "1000", []string{"998"}, SocketOwner{UID: 1000, GID: 998, Mode: 0o060}, false},
		{"world writable", "1001", nil, SocketOwner{Mode: 0o666}, true},
	}
	for _, c := range cases {
		if got := socketAllows(c.uid, c.gids, c.s); got != c.want {
			t.Errorf("%s: socketAllows = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAnyVerifier(t *testing.T) {
	no := verifierFunc(func(string, string) bool { return false })
	yes := verifierFunc(func(u, p string) bool { return u == "a" && p == "b" })
	if !(Any{no, yes}).Verify("a", "b") || (Any{no, yes}).Verify("a", "c") || (Any{}).Verify("a", "b") {
		t.Error("Any does not accept exactly what one of its verifiers accepts")
	}
}

type verifierFunc func(string, string) bool

func (f verifierFunc) Verify(u, p string) bool { return f(u, p) }
