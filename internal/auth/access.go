package auth

import (
	"io/fs"
	"slices"
	"strconv"
)

// Verifier checks sign-in credentials. Users (config file accounts) and
// System (Linux and Windows system accounts) implement it.
type Verifier interface {
	Verify(username, password string) bool
}

// Any accepts the credentials one of its verifiers accepts, asked in order.
type Any []Verifier

func (a Any) Verify(username, password string) bool {
	for _, v := range a {
		if v.Verify(username, password) {
			return true
		}
	}
	return false
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
	const rw = 0o6
	if uid == "0" {
		return true
	}
	if uid == strconv.FormatUint(uint64(s.UID), 10) {
		return s.Mode.Perm()>>6&rw == rw
	}
	if slices.Contains(gids, strconv.FormatUint(uint64(s.GID), 10)) {
		return s.Mode.Perm()>>3&rw == rw
	}
	return s.Mode.Perm()&rw == rw
}
