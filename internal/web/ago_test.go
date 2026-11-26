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

func TestHealthStatus(t *testing.T) {
	for status, want := range map[string][2]string{
		"Up 4 hours (healthy)":            {"healthy", "Up 4 hours"},
		"Up 2 minutes (unhealthy)":        {"unhealthy", "Up 2 minutes"},
		"Up 3 seconds (health: starting)": {"starting", "Up 3 seconds"},
		"Up 26 hours":                     {"none", "Up 26 hours"},
		"Exited (0) 40 hours ago":         {"none", "Exited (0) 40 hours ago"},
	} {
		if h, u := health(status), upStatus(status); h != want[0] || u != want[1] {
			t.Errorf("%q: health %q, up %q; want %q, %q", status, h, u, want[0], want[1])
		}
	}
}
