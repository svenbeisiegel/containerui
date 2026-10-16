package events

import "time"

// journalFollowArgs are the journalctl arguments to follow the error
// entries after cursor, or from since without one.
func journalFollowArgs(cursor string, since time.Time) []string {
	args := []string{"--priority=err", "--output=json", "--follow", "--lines=all", "--no-pager", "--quiet"}
	if cursor != "" {
		return append(args, "--after-cursor="+cursor)
	}
	return append(args, "--since="+since.Local().Format("2006-01-02 15:04:05"))
}
