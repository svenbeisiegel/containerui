// Package dockerapi reads resource usage straight from the Docker Engine
// API on the daemon's Unix socket (podman's docker-compatible socket works
// too). Asking the API costs the daemon far less than the CLI: no process
// per sample, and the stats are read once instead of twice a second apart.
// Only the endpoints the resource monitoring needs are implemented; the
// rest of ContainerUI uses the CLI. NewStater falls back to the CLI when
// the socket cannot be used.
package dockerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxBody bounds a response (system df lists every image and volume).
const maxBody = 64 << 20

// Client talks to the Engine API on a Unix socket. Requests use unversioned
// paths, so the daemon answers in its own (newest) API version.
type Client struct {
	socket string
	http   *http.Client

	mu   sync.Mutex
	prev map[string]cpuSample // last CPU counters by container ID, see Stats
}

// New returns a client for the Unix socket at path. It does not connect.
func New(path string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     2 * time.Minute,
	}
	return &Client{socket: path, http: &http.Client{Transport: tr}, prev: map[string]cpuSample{}}
}

// Socket is the path the client connects to.
func (c *Client) Socket() string { return c.socket }

// Error is a non-2xx answer of the daemon.
type Error struct {
	Path    string
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("docker API %s: %d %s", e.Path, e.Status, e.Message)
}

// IsNotFound reports whether err is a 404 of the daemon (e.g. a container
// removed between listing and asking for its stats).
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// get requests path and decodes the JSON answer into v.
func (c *Client) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker API %s: %w", path, err)
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, maxBody)
	if resp.StatusCode/100 != 2 {
		var e struct{ Message string }
		data, _ := io.ReadAll(io.LimitReader(body, 64<<10))
		if json.Unmarshal(data, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		return &Error{Path: path, Status: resp.StatusCode, Message: e.Message}
	}
	if err := json.NewDecoder(body).Decode(v); err != nil {
		return fmt.Errorf("docker API %s: decode: %w", path, err)
	}
	return nil
}

// Ping checks that the daemon answers on the socket.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker API /_ping: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return &Error{Path: "/_ping", Status: resp.StatusCode}
	}
	return nil
}

// Close drops idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }
