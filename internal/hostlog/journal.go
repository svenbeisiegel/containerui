package hostlog

import (
	"slices"
	"strconv"
	"strings"
)

var journalPriorities = []Option{
	{"", "All priorities"},
	{"err", "Error and worse"},
	{"warning", "Warning and worse"},
	{"notice", "Notice and worse"},
	{"info", "Info and worse"},
}

var journalRanges = []Option{
	{"boot", "Current boot"},
	{"1h", "Last hour"},
	{"24h", "Last 24 hours"},
	{"7d", "Last 7 days"},
	{"", "Entire journal"},
}

// journalPinned are listed first when the journal has entries for them.
var journalPinned = []string{"containerui.service", "docker.service", "containerd.service", "podman.service"}

// journalArgs builds the journalctl arguments for a validated query.
func journalArgs(q Query) []string {
	args := []string{"--no-pager", "--output=short-iso"}
	if q.Selector != "" {
		args = append(args, "--unit="+q.Selector)
	}
	if q.Priority != "" {
		args = append(args, "--priority="+q.Priority)
	}
	switch q.Since {
	case "":
	case "boot":
		args = append(args, "--boot")
	default:
		args = append(args, "--since=-"+q.Since)
	}
	if q.Follow {
		n := "all"
		if q.Tail > 0 {
			n = strconv.Itoa(q.Tail)
		}
		args = append(args, "--follow", "--lines="+n)
	}
	return args
}

// journalUnits turns `journalctl -F _SYSTEMD_UNIT` output into options:
// all units first, then the pinned units present, then the rest sorted.
func journalUnits(out string) []Option {
	var units []string
	for _, u := range strings.Split(out, "\n") {
		if u = strings.TrimSpace(u); u != "" && !strings.HasPrefix(u, "-") {
			units = append(units, u)
		}
	}
	slices.Sort(units)
	units = slices.Compact(units)
	opts := []Option{{"", "All units"}}
	for _, p := range journalPinned {
		if slices.Contains(units, p) {
			opts = append(opts, Option{p, p})
		}
	}
	for _, u := range units {
		if !slices.Contains(journalPinned, u) {
			opts = append(opts, Option{u, u})
		}
	}
	return opts
}
