package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	ErrUnauthorized = errors.New("access denied by the registry (log in with docker/podman login on this host)")
	ErrNotFound     = errors.New("not found in the registry")
)

// manifestAccept lists the manifest types a digest is asked for: the
// index / manifest list first, as "docker pull" records its digest.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
}, ", ")

// maxTagPages bounds the tag list pagination.
const maxTagPages = 20

// Client talks to registries.
type Client struct {
	HTTP        *http.Client
	Credentials Credentials // nil = anonymous
	// Insecure lists registries reached over plain HTTP.
	Insecure []string

	mu     sync.Mutex
	tokens map[string]token // by registry + scope
}

type token struct {
	value   string
	expires time.Time
}

// NewClient creates a client with a per-request timeout.
func NewClient(timeout time.Duration, creds Credentials, insecure []string) *Client {
	return &Client{HTTP: &http.Client{Timeout: timeout}, Credentials: creds, Insecure: insecure}
}

func (c *Client) baseURL(r Reference) string {
	scheme := "https"
	if slices.Contains(c.Insecure, r.Registry) {
		scheme = "http"
	}
	return scheme + "://" + r.apiHost()
}

// Digest returns the digest the registry serves for ref's tag.
func (c *Client) Digest(ctx context.Context, ref Reference) (string, error) {
	u := c.baseURL(ref) + "/v2/" + ref.Repository + "/manifests/" + ref.Tag
	resp, err := c.do(ctx, http.MethodHead, u, ref, manifestAccept)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" {
		return d, nil
	}
	// Not every registry sends the header on HEAD: hash the manifest.
	resp, err = c.do(ctx, http.MethodGet, u, ref, manifestAccept)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" {
		return d, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(resp.Body, 4<<20)); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

var linkRe = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

// Tags lists the repository's tags.
func (c *Client) Tags(ctx context.Context, ref Reference) ([]string, error) {
	base := c.baseURL(ref)
	next := base + "/v2/" + ref.Repository + "/tags/list?n=1000"
	var tags []string
	for page := 0; next != "" && page < maxTagPages; page++ {
		resp, err := c.do(ctx, http.MethodGet, next, ref, "application/json")
		if err != nil {
			return nil, err
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("tag list: %w", err)
		}
		tags = append(tags, body.Tags...)
		next = ""
		if m := linkRe.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
			nu, err := url.Parse(m[1])
			if err != nil {
				break
			}
			b, _ := url.Parse(base)
			next = b.ResolveReference(nu).String()
		}
	}
	return tags, nil
}

// do sends a request, answering the registry's auth challenge once.
func (c *Client) do(ctx context.Context, method, u string, ref Reference, accept string) (*http.Response, error) {
	scope := "repository:" + ref.Repository + ":pull"
	key := ref.Registry + " " + scope
	send := func(auth string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", accept)
		req.Header.Set("User-Agent", "ContainerUI")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		return c.HTTP.Do(req)
	}
	auth := ""
	c.mu.Lock()
	if t, ok := c.tokens[key]; ok && time.Now().Before(t.expires) {
		auth = "Bearer " + t.value
	}
	c.mu.Unlock()
	resp, err := send(auth)
	if err != nil {
		return nil, c.netError(ref, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		auth, err = c.authorize(ctx, ref, challenge, scope, key)
		if err != nil {
			return nil, err
		}
		if resp, err = send(auth); err != nil {
			return nil, c.netError(ref, err)
		}
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		resp.Body.Close()
		return nil, ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		return nil, ErrNotFound
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("registry answered %s", resp.Status)
	}
}

// authorize answers a WWW-Authenticate challenge and returns the
// Authorization header to retry with.
func (c *Client) authorize(ctx context.Context, ref Reference, challenge, scope, key string) (string, error) {
	user, pass := "", ""
	if c.Credentials != nil {
		var err error
		if user, pass, err = c.Credentials(ref.Registry); err != nil {
			return "", err
		}
	}
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		if user == "" {
			return "", ErrUnauthorized
		}
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.SetBasicAuth(user, pass)
		return req.Header.Get("Authorization"), nil
	case "bearer":
		realm := params["realm"]
		ru, err := url.Parse(realm)
		if err != nil || (ru.Scheme != "https" && ru.Scheme != "http") {
			return "", fmt.Errorf("registry sent an invalid token realm %q", realm)
		}
		q := ru.Query()
		if s := params["service"]; s != "" {
			q.Set("service", s)
		}
		q.Set("scope", scope)
		ru.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ru.String(), nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", "ContainerUI")
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return "", fmt.Errorf("get a registry token: %w", c.netError(ref, err))
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", ErrUnauthorized
		}
		var body struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
			return "", fmt.Errorf("registry token: %w", err)
		}
		t := body.Token
		if t == "" {
			t = body.AccessToken
		}
		if t == "" {
			return "", ErrUnauthorized
		}
		ttl := time.Duration(body.ExpiresIn) * time.Second
		if ttl < time.Minute {
			ttl = time.Minute
		}
		c.mu.Lock()
		if c.tokens == nil {
			c.tokens = map[string]token{}
		}
		c.tokens[key] = token{value: t, expires: time.Now().Add(ttl - 10*time.Second)}
		c.mu.Unlock()
		return "Bearer " + t, nil
	}
	return "", ErrUnauthorized
}

// netError shortens transport errors for display.
func (c *Client) netError(ref Reference, err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	if ue.Timeout() {
		return fmt.Errorf("%s did not answer within %s", ref.apiHost(), c.HTTP.Timeout)
	}
	return fmt.Errorf("cannot reach %s: %w", ref.apiHost(), ue.Err)
}

// parseChallenge splits `Bearer realm="...",service="..."`.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if strings.HasPrefix(v, `"`) {
			end := strings.Index(v[1:], `"`)
			if end < 0 {
				params[k] = v[1:]
				break
			}
			params[k], rest = v[1:end+1], v[end+2:]
		} else {
			val, after, _ := strings.Cut(v, ",")
			params[k], rest = strings.TrimSpace(val), after
		}
	}
	return scheme, params
}
