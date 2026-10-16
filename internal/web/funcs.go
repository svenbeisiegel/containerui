package web

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/svenbeisiegel/containerui/internal/compose"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

var funcs = template.FuncMap{
	"shortID":   shortID,
	"hasErrors": compose.HasErrors,
	"ago":       ago,
	"join":      strings.Join,
	"stateClass": func(s runtime.State) string {
		switch s {
		case runtime.StateRunning:
			return "ok"
		case runtime.StatePaused:
			return "info"
		case runtime.StateRestarting, runtime.StateCreated:
			return "warn"
		case runtime.StateDead:
			return "bad"
		}
		return "muted"
	},
	"dict": func(kv ...any) (map[string]any, error) {
		if len(kv)%2 != 0 {
			return nil, fmt.Errorf("dict: odd number of arguments")
		}
		m := make(map[string]any, len(kv)/2)
		for i := 0; i < len(kv); i += 2 {
			k, ok := kv[i].(string)
			if !ok {
				return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
			}
			m[k] = kv[i+1]
		}
		return m, nil
	},
	"bytes":    humanBytes,
	"duration": humanDuration,
	"datetime": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Local().Format("2006-01-02 15:04:05")
	},
}

// shortID shortens a container/image ID to 12 characters, dropping the
// digest algorithm prefix.
func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "–"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 60*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	case d < 730*24*time.Hour:
		return plural(int(d.Hours()/24/30), "month") + " ago"
	}
	return plural(int(d.Hours()/24/365), "year") + " ago"
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// humanBytes formats n with binary units: "15.5 GiB".
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// humanDuration formats a long duration coarsely: "11 days 2 hours",
// "3 hours 5 minutes", "42 minutes".
func humanDuration(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return plural(days, "day") + " " + plural(hours, "hour")
	case hours > 0:
		return plural(hours, "hour") + " " + plural(mins, "minute")
	}
	return plural(mins, "minute")
}
