package auth

import (
	"io/fs"
	"slices"
	"strconv"
)

// Verifier checks sign-in credentials. Users (config file accounts) and
// System (Linux and Windows system accounts) implement it.
//
// Verify returns the account the credentials belong to: a key that is the
// same however the user spelled the name (the canonical Linux user name, the
// Windows account SID, the config user name). Per-account state such as the
// second factor is kept under it.
type Verifier interface {
	Verify(username, password string) (account string, ok bool)
}

// Any accepts the credentials one of its verifiers accepts, asked in order.
type Any []Verifier

func (a Any) Verify(username, password string) (string, bool) {
	for _, v := range a {
		if account, ok := v.Verify(username, password); ok {
			return account, true
		}
	}
	return "", false
}

// SocketOwner describes a Unix socket's owner and permission bits.
type SocketOwner struct {
	UID, GID uint32
	Mode     fs.FileMode
}

// socketAllows reports whether a user (uid, group ids) may read and write a
// socket with the given owner and mode, as the kernel would decide it for
// connect(2). root may always.
func socketAllows(uid string, gids []string, s SocketOwner) bool {
	return modeAllows(uid, gids, s, 0o6)
}

// fileReadable reports whether a user may read a file with the given owner
// and mode (its own permission bits; the folders above it are not checked).
// root may always.
func fileReadable(uid string, gids []string, s SocketOwner) bool {
	return modeAllows(uid, gids, s, 0o4)
}

// modeAllows applies the owner, group and other permission bits like the
// kernel: the first class the user belongs to decides.
func modeAllows(uid string, gids []string, s SocketOwner, want fs.FileMode) bool {
	if uid == "0" {
		return true
	}
	if uid == strconv.FormatUint(uint64(s.UID), 10) {
		return s.Mode.Perm()>>6&want == want
	}
	if slices.Contains(gids, strconv.FormatUint(uint64(s.GID), 10)) {
		return s.Mode.Perm()>>3&want == want
	}
	return s.Mode.Perm()&want == want
}
