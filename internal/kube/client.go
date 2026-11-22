package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// maxBody bounds a non-streamed response (one list page is at most
// pageSize objects).
const maxBody = 64 << 20

// pageSize is the limit of one list request; List follows "continue".
const pageSize = 500

// tokenTTL is how long a token read from a file is reused: Kubernetes
// rotates projected service account tokens well before they expire.
const tokenTTL = time.Minute

var (
	// ErrGone: the resource version a watch or list continuation started
	// from is too old (HTTP 410); list again.
	ErrGone = errors.New("resource version expired")
	// ErrForbidden: the account may not do this (HTTP 403, RBAC).
	ErrForbidden = errors.New("forbidden")
	// ErrUnauthorized: the API server did not accept the credentials (401).
	ErrUnauthorized = errors.New("unauthorized")
)

// Options tune a Client.
type Options struct {
	UserAgent string
	// Timeout bounds Get and List when ctx has no deadline (0 = 30 s).
	// Watches and streams are not bounded by it.
	Timeout time.Duration
}

// Client talks to one API server.
type Client struct {
	cfg       *Config
	http      *http.Client
	userAgent string
	timeout   time.Duration

	mu        sync.Mutex
	token     string
	tokenRead time.Time
}

// New returns a client for cfg. It does not connect.
func New(cfg *Config, o Options) (*Client, error) {
	tlsCfg, err := tlsConfig(cfg)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     tlsCfg,
		TLSHandshakeTimeout: 10 * time.Second,
		// One HTTP/2 connection carries all watches and requests.
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     2 * time.Minute,
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	if o.UserAgent == "" {
		o.UserAgent = "containerui"
	}
	return &Client{cfg: cfg, http: &http.Client{Transport: tr}, userAgent: o.UserAgent, timeout: o.Timeout}, nil
}

func tlsConfig(cfg *Config) (*tls.Config, error) {
	t := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.ServerName, InsecureSkipVerify: cfg.Insecure} //nolint:gosec // only when the kubeconfig asks for it
	if len(cfg.CAData) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cfg.CAData) {
			return nil, errors.New("kubernetes: certificate authority holds no PEM certificate")
		}
		t.RootCAs = pool
	}
	if len(cfg.CertData) > 0 {
		cert, err := tls.X509KeyPair(cfg.CertData, cfg.KeyData)
		if err != nil {
			return nil, fmt.Errorf("kubernetes: client certificate: %w", err)
		}
		t.Certificates = []tls.Certificate{cert}
	}
	return t, nil
}

// Config returns the settings the client was created with.
func (c *Client) Config() *Config { return c.cfg }

// Close drops idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// Path joins escaped path segments: Path("api", "v1", "namespaces", ns, "pods").
func Path(segments ...string) string {
	var b strings.Builder
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	return b.String()
}

// Error is a failed API request. It unwraps to runtime.ErrNotFound,
// runtime.ErrConflict, runtime.ErrInvalid, ErrGone, ErrForbidden or
// ErrUnauthorized where the status code fits.
type Error struct {
	Method  string
	Path    string
	Status  int    // HTTP status (or the code of a watch ERROR event)
	Reason  string // metav1.Status reason, e.g. NotFound, Forbidden, Expired
	Message string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("kubernetes API %s %s: %d", e.Method, e.Path, e.Status)
	if e.Reason != "" {
		msg += " " + e.Reason
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

func (e *Error) Unwrap() error {
	switch e.Status {
	case http.StatusNotFound:
		return runtime.ErrNotFound
	case http.StatusConflict:
		return runtime.ErrConflict
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return runtime.ErrInvalid
	case http.StatusGone:
		return ErrGone
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusUnauthorized:
		return ErrUnauthorized
	}
	return nil
}

// status is metav1.Status, the body of every API error.
type status struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
	Code    int    `json:"code"`
}

func (c *Client) bearer() (string, error) {
	if c.cfg.Token != "" || c.cfg.TokenFile == "" {
		return c.cfg.Token, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Since(c.tokenRead) < tokenTTL {
		return c.token, nil
	}
	b, err := os.ReadFile(c.cfg.TokenFile)
	if err != nil {
		if c.token != "" {
			return c.token, nil // keep the last one; the file may be mid-rotation
		}
		return "", fmt.Errorf("kubernetes: token file: %w", err)
	}
	c.token, c.tokenRead = strings.TrimSpace(string(b)), time.Now()
	return c.token, nil
}

// do sends a request and returns the response if its status is 2xx; any
// other answer is turned into an *Error.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := c.cfg.Server + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	tok, err := c.bearer()
	if err != nil {
		return nil, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("kubernetes API %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &Error{Method: method, Path: path, Status: resp.StatusCode}
	var st status
	if json.Unmarshal(data, &st) == nil && st.Kind == "Status" {
		e.Reason, e.Message = st.Reason, st.Message
	} else {
		e.Message = strings.TrimSpace(string(data))
	}
	return nil, e
}

// bounded applies the client's timeout to ctx if it has no deadline.
func (c *Client) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.timeout)
}

// Get requests path and decodes the JSON answer into v (a
// *json.RawMessage keeps it as it is).
func (c *Client) Get(ctx context.Context, path string, q url.Values, v any) error {
	ctx, cancel := c.bounded(ctx)
	defer cancel()
	resp, err := c.do(ctx, http.MethodGet, path, q, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(v); err != nil {
		return fmt.Errorf("kubernetes API GET %s: decode: %w", path, err)
	}
	return nil
}

// List reads every item of a collection, following "continue" across
// pages, and returns them undecoded with the list's resource version (the
// point a Watch continues from). An expired continuation fails with
// ErrGone; list again.
func (c *Client) List(ctx context.Context, path string, q url.Values) (items []json.RawMessage, resourceVersion string, err error) {
	query := url.Values{}
	for k, v := range q {
		query[k] = v
	}
	query.Set("limit", fmt.Sprint(pageSize))
	for {
		var page struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
				Continue        string `json:"continue"`
			} `json:"metadata"`
			Items []json.RawMessage `json:"items"`
		}
		if err := c.Get(ctx, path, query, &page); err != nil {
			return nil, "", err
		}
		items = append(items, page.Items...)
		resourceVersion = page.Metadata.ResourceVersion
		if page.Metadata.Continue == "" {
			return items, resourceVersion, nil
		}
		query.Set("continue", page.Metadata.Continue)
	}
}

// Watch event types.
const (
	Added    = "ADDED"
	Modified = "MODIFIED"
	Deleted  = "DELETED"
	Bookmark = "BOOKMARK" // only the object's resourceVersion is set
)

// WatchEvent is one change of a watched collection.
type WatchEvent struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

// Watch streams the changes of a collection to fn, starting after
// q["resourceVersion"] (usually from List), with bookmarks on. It returns
// nil when the server ends the watch (after q["timeoutSeconds"] or its own
// limit): watch again from the last resource version seen. A too old
// resource version fails with ErrGone; list again. An error of fn ends the
// watch and is returned.
func (c *Client) Watch(ctx context.Context, path string, q url.Values, fn func(WatchEvent) error) error {
	query := url.Values{}
	for k, v := range q {
		query[k] = v
	}
	query.Set("watch", "1")
	query.Set("allowWatchBookmarks", "true")
	resp, err := c.do(ctx, http.MethodGet, path, query, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var ev WatchEvent
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return fmt.Errorf("kubernetes API watch %s: %w", path, err)
		}
		if ev.Type == "ERROR" {
			var st status
			_ = json.Unmarshal(ev.Object, &st)
			return &Error{Method: http.MethodGet, Path: path, Status: st.Code, Reason: st.Reason, Message: st.Message}
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
}

// Stream requests path and returns the body to read as it arrives (pod
// logs). The caller closes it; cancelling ctx ends it.
func (c *Client) Stream(ctx context.Context, path string, q url.Values) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, path, q, nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Version is the API server's /version answer.
type Version struct {
	Major      string `json:"major"`
	Minor      string `json:"minor"`
	GitVersion string `json:"gitVersion"`
	Platform   string `json:"platform"`
}

// Version asks the API server for its version. Anonymous clients may ask
// too, so it checks connectivity, not the credentials (see WhoAmI).
func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	err := c.Get(ctx, "/version", nil, &v)
	return v, err
}

// UserInfo is the identity the API server sees for the client's credentials.
type UserInfo struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups"`
}

// WhoAmI asks the API server who the credentials authenticate as (a
// SelfSubjectReview, Kubernetes 1.28+), which checks them.
func (c *Client) WhoAmI(ctx context.Context) (UserInfo, error) {
	ctx, cancel := c.bounded(ctx)
	defer cancel()
	const path = "/apis/authentication.k8s.io/v1/selfsubjectreviews"
	body := strings.NewReader(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`)
	resp, err := c.do(ctx, http.MethodPost, path, nil, body, "application/json")
	if err != nil {
		return UserInfo{}, err
	}
	defer resp.Body.Close()
	var r struct {
		Status struct {
			UserInfo UserInfo `json:"userInfo"`
		} `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return UserInfo{}, fmt.Errorf("kubernetes API POST %s: decode: %w", path, err)
	}
	return r.Status.UserInfo, nil
}
