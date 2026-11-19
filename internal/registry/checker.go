package registry

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Status is the result of an update check for an image reference.
type Status struct {
	Ref string
	// Err explains why the check failed or could not be made.
	Err string
	// Pinned: the reference names a digest, so it never changes.
	Pinned bool
	// DigestChanged: the registry serves a different image for the tag
	// than the local one.
	DigestChanged bool
	// DigestUnknown: the local image has no registry digest to compare
	// (built or loaded locally), so a rebuilt tag cannot be detected.
	DigestUnknown bool
	// TagGone: the registry no longer has the tag, though it still has
	// the repository (the tag was deleted there). Newer tags are still
	// looked for.
	TagGone bool
	// NewerTags are higher version tags of the same shape.
	NewerTags []Tag
	CheckedAt time.Time
}

// UpdateAvailable reports whether the tag was rebuilt or newer tags exist.
func (s Status) UpdateAvailable() bool { return s.DigestChanged || len(s.NewerTags) > 0 }

// UpdateChecker checks image references for updates.
type UpdateChecker interface {
	// Check compares ref with the registry. localDigests are the local
	// image's repo digests (RepoDigests of image inspect). force skips the
	// cache.
	Check(ctx context.Context, ref string, localDigests []string, force bool) Status
}

// Checker is an UpdateChecker that caches what registries answer.
type Checker struct {
	Client *Client
	TTL    time.Duration

	mu    sync.Mutex
	cache map[string]remote // by normalized reference
	now   func() time.Time
}

// errorTTL is how long a failed check is reused: network trouble is
// often brief.
const errorTTL = time.Minute

type remote struct {
	digest string
	gone   bool // the tag is not in the registry, the repository is
	tags   []string
	err    error
	at     time.Time
}

func NewChecker(c *Client, ttl time.Duration) *Checker {
	return &Checker{Client: c, TTL: ttl, cache: map[string]remote{}, now: time.Now}
}

func (c *Checker) Check(ctx context.Context, refStr string, localDigests []string, force bool) Status {
	s := Status{Ref: refStr}
	ref, err := ParseRef(refStr)
	if err != nil {
		s.Err = err.Error()
		return s
	}
	if ref.Digest != "" {
		s.Pinned = true
		return s
	}
	r := c.lookup(ctx, ref, force)
	s.CheckedAt = r.at
	if r.err != nil {
		s.Err = r.err.Error()
		return s
	}
	s.NewerTags = NewerTags(ref.Tag, r.tags)
	if r.gone {
		// Nothing to compare the local image with.
		s.TagGone = true
		return s
	}
	s.DigestUnknown = true
	for _, ld := range localDigests {
		l, err := ParseRef(ld)
		if err != nil || l.Name() != ref.Name() {
			continue
		}
		if l.Digest == r.digest {
			s.DigestUnknown, s.DigestChanged = false, false
			break
		}
		s.DigestUnknown, s.DigestChanged = false, true
	}
	return s
}

func (c *Checker) lookup(ctx context.Context, ref Reference, force bool) remote {
	key := ref.String()
	c.mu.Lock()
	r, ok := c.cache[key]
	c.mu.Unlock()
	ttl := c.TTL
	if r.err != nil {
		ttl = min(ttl, errorTTL)
	}
	if ok && !force && c.now().Sub(r.at) < ttl {
		return r
	}
	r = remote{at: c.now()}
	r.digest, r.err = c.Client.Digest(ctx, ref)
	switch {
	case r.err == nil:
		// Some registries do not allow listing tags; the digest check
		// still works then.
		r.tags, _ = c.Client.Tags(ctx, ref)
	case errors.Is(r.err, ErrNotFound):
		// The tag may have been deleted while newer ones were pushed: the
		// tag list tells, and fails too when the repository is unknown.
		if tags, err := c.Client.Tags(ctx, ref); err == nil {
			r.tags, r.gone, r.err = tags, true, nil
		}
	}
	// Answers to cancelled requests are not cached.
	if ctx.Err() == nil {
		c.mu.Lock()
		c.cache[key] = r
		c.mu.Unlock()
	}
	return r
}
