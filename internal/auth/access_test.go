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
	ok := func(v Verifier, u, p string) bool { _, ok := v.Verify(u, p); return ok }
	if !ok(Any{no, yes}, "a", "b") || ok(Any{no, yes}, "a", "c") || ok(Any{}, "a", "b") {
		t.Error("Any does not accept exactly what one of its verifiers accepts")
	}
	if account, _ := (Any{no, yes}).Verify("a", "b"); account != "account:a" {
		t.Errorf("Any returned account %q", account)
	}
}

type verifierFunc func(string, string) bool

func (f verifierFunc) Verify(u, p string) (string, bool) {
	if f(u, p) {
		return "account:" + u, true
	}
	return "", false
}

func TestFileReadable(t *testing.T) {
	kubeconfig := SocketOwner{UID: 0, GID: 990, Mode: 0o640} // root:containerui -rw-r-----
	for _, c := range []struct {
		name string
		uid  string
		gids []string
		want bool
	}{
		{"root", "0", nil, true},
		{"group member", "1000", []string{"100", "990"}, true},
		{"other", "1000", []string{"100"}, false},
	} {
		if got := fileReadable(c.uid, c.gids, kubeconfig); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	// RKE2's default rke2.yaml is 0644: everyone may read it.
	if !fileReadable("1000", nil, SocketOwner{UID: 0, GID: 0, Mode: 0o644}) {
		t.Error("a world-readable file must be readable")
	}
	// The owner class decides even if others may read.
	if fileReadable("1000", nil, SocketOwner{UID: 1000, GID: 0, Mode: 0o044}) {
		t.Error("owner without read permission must not read")
	}
}
