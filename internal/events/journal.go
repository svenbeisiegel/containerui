package events

import "time"

// journalFollowArgs are the journalctl arguments to follow the error
// entries from since on. since is given in UTC, so it is exact whatever
// the time zone and daylight saving time.
func journalFollowArgs(since time.Time) []string {
	return []string{"--priority=err", "--output=short-iso-precise", "--all", "--follow", "--lines=all", "--no-pager", "--quiet",
		"--since=" + since.UTC().Format("2006-01-02 15:04:05.000000") + " UTC"}
}
