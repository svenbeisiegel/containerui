package registry

import (
	"regexp"
	"sort"
	"strconv"
)

// Tag is a version tag newer than the current one.
type Tag struct {
	Name string
	Kind string // major, minor or patch: the highest part that changed
}

// MaxNewerTags is how many newer tags NewerTags returns.
const MaxNewerTags = 5

var versionRe = regexp.MustCompile(`^(v?)(\d+)(?:\.(\d+))?(?:\.(\d+))?(-[A-Za-z0-9._-]+)?$`)

type version struct {
	prefix, suffix string
	parts          []int
}

func parseVersion(tag string) (version, bool) {
	m := versionRe.FindStringSubmatch(tag)
	if m == nil {
		return version{}, false
	}
	v := version{prefix: m[1], suffix: m[5]}
	for _, p := range m[2:5] {
		if p == "" {
			break
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return version{}, false
		}
		v.parts = append(v.parts, n)
	}
	return v, true
}

func (v version) sameShape(o version) bool {
	return v.prefix == o.prefix && v.suffix == o.suffix && len(v.parts) == len(o.parts)
}

// compare returns <0, 0, >0 and the index of the first differing part.
func (v version) compare(o version) (int, int) {
	for i := range v.parts {
		if v.parts[i] != o.parts[i] {
			return v.parts[i] - o.parts[i], i
		}
	}
	return 0, -1
}

// NewerTags returns the tags of the same shape as current (same "v"
// prefix, number of parts and suffix such as "-alpine") that are higher,
// newest first, at most MaxNewerTags. A current tag that is no version
// (latest, stable, …) has none.
func NewerTags(current string, tags []string) []Tag {
	cur, ok := parseVersion(current)
	if !ok {
		return nil
	}
	type cand struct {
		name string
		v    version
		kind string
	}
	var cands []cand
	seen := map[string]bool{}
	for _, t := range tags {
		v, ok := parseVersion(t)
		if !ok || !v.sameShape(cur) || seen[t] {
			continue
		}
		seen[t] = true
		if d, i := v.compare(cur); d > 0 {
			cands = append(cands, cand{t, v, [...]string{"major", "minor", "patch"}[i]})
		}
	}
	sort.Slice(cands, func(a, b int) bool { d, _ := cands[a].v.compare(cands[b].v); return d > 0 })
	var out []Tag
	for _, c := range cands[:min(len(cands), MaxNewerTags)] {
		out = append(out, Tag{Name: c.name, Kind: c.kind})
	}
	return out
}
