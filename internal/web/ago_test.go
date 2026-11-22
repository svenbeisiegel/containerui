package web

import (
	"testing"
	"time"
)

func TestAgo(t *testing.T) {
	day := 24 * time.Hour
	for d, want := range map[time.Duration]string{
		30 * time.Second: "just now",
		45 * time.Minute: "45 minutes ago",
		3 * time.Hour:    "3 hours ago",
		4 * day:          "4 days ago",
		13 * day:         "13 days ago",
		15 * day:         "2 weeks ago",
		29 * day:         "4 weeks ago",
		90 * day:         "3 months ago",
	} {
		if got := ago(time.Now().Add(-d)); got != want {
			t.Errorf("ago(-%v) = %q, want %q", d, got, want)
		}
	}
}
