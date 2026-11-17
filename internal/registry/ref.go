// Package registry asks container registries (Docker Registry HTTP API
// v2 / OCI distribution) whether a newer image exists for a reference: a
// new digest for the same tag, or higher version tags.
package registry

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// DockerHub is the canonical name of Docker Hub.
const DockerHub = "docker.io"

// Reference is a parsed image reference.
type Reference struct {
	Registry   string // e.g. docker.io, ghcr.io, localhost:5000
	Repository string // e.g. library/nginx
	Tag        string // "latest" if the reference has neither tag nor digest
	Digest     string // sha256:..., for digest references
}

// ErrNotRegistry is returned for references that name no registry image
// (image IDs).
var ErrNotRegistry = errors.New("not a registry image reference")

var (
	repoRe   = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	tagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRe = regexp.MustCompile(`^[a-z0-9]+(?:[+._-][a-z0-9]+)*:[a-fA-F0-9]{32,}$`)
	idRe     = regexp.MustCompile(`^(sha256:)?[a-f0-9]{12,64}$`)
)

// ParseRef parses "[registry/]repository[:tag][@digest]". Docker Hub
// references are normalized (nginx → docker.io/library/nginx).
func ParseRef(ref string) (Reference, error) {
	var r Reference
	s := strings.TrimSpace(ref)
	if idRe.MatchString(s) {
		return r, ErrNotRegistry
	}
	if name, digest, ok := strings.Cut(s, "@"); ok {
		if !digestRe.MatchString(digest) {
			return r, fmt.Errorf("invalid digest in %q", ref)
		}
		s, r.Digest = name, digest
	}
	if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "/") {
		s, r.Tag = s[:i], s[i+1:]
		if !tagRe.MatchString(r.Tag) {
			return r, fmt.Errorf("invalid tag in %q", ref)
		}
	}
	if first, rest, ok := strings.Cut(s, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Registry, s = strings.ToLower(first), rest
	} else {
		r.Registry = DockerHub
	}
	if r.Registry == "index.docker.io" || r.Registry == "registry-1.docker.io" {
		r.Registry = DockerHub
	}
	if r.Registry == DockerHub && !strings.Contains(s, "/") {
		s = "library/" + s
	}
	if !repoRe.MatchString(s) {
		return r, fmt.Errorf("invalid repository in %q", ref)
	}
	r.Repository = s
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r, nil
}

// Name is registry/repository.
func (r Reference) Name() string { return r.Registry + "/" + r.Repository }

// String renders the normalized reference.
func (r Reference) String() string {
	s := r.Name()
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// apiHost is the host serving the registry API.
func (r Reference) apiHost() string {
	if r.Registry == DockerHub {
		return "registry-1.docker.io"
	}
	return r.Registry
}
