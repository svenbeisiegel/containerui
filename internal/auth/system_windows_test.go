package auth

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSplitAccount(t *testing.T) {
	for _, tc := range []struct{ in, user, domain string }{
		{"alice", "alice", ""},
		{`CORP\alice`, "alice", "CORP"},
		{`.\alice`, "alice", "."},
		{"alice@corp.example", "alice@corp.example", ""},
	} {
		if u, d := splitAccount(tc.in); u != tc.user || d != tc.domain {
			t.Errorf("splitAccount(%q) = %q, %q; want %q, %q", tc.in, u, d, tc.user, tc.domain)
		}
	}
}

func processToken(t *testing.T) windows.Token {
	t.Helper()
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &token); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { token.Close() })
	return token
}

// Without a pipe, the account ContainerUI runs as may sign in.
func TestSystemAllowsProcessUserWithoutPipe(t *testing.T) {
	s := &System{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := s.mayUseRuntime(processToken(t)); err != nil {
		t.Fatalf("mayUseRuntime: %v", err)
	}
}

// Verify passes the account apart and refuses what LogonUser refuses.
func TestSystemVerify(t *testing.T) {
	var gotUser, gotDomain string
	s := &System{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.logon = func(user, domain, password string) (windows.Token, error) {
		gotUser, gotDomain = user, domain
		if password != "right" {
			return 0, errors.New("logon failure")
		}
		var token windows.Token
		err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &token)
		return token, err
	}
	ok := func(u, p string) bool { _, ok := s.Verify(u, p); return ok }
	if ok(`CORP\alice`, "wrong") {
		t.Error("a wrong password was accepted")
	}
	if gotUser != "alice" || gotDomain != "CORP" {
		t.Errorf("logon got %q, %q", gotUser, gotDomain)
	}
	if account, _ := s.Verify("alice", "right"); !strings.HasPrefix(account, "S-1-") {
		t.Errorf("the account ContainerUI runs as was refused or has no SID: %q", account)
	}
	if ok("", "right") || ok("alice", "") || ok("a\x00b", "right") {
		t.Error("malformed credentials were accepted")
	}
}
