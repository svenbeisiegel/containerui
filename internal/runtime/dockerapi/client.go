// Package dockerapi talks to the Docker Engine API on the daemon's Unix
// socket (podman's docker-compatible socket works too). Asking the API
// costs the daemon far less than the CLI: no process per call, typed JSON
// instead of CLI output, and streams (logs, pulls, terminals) without a
// CLI process in between.
//
// Driver wraps the CLI driver: it reads lists, inspect documents, logs,
// pulls, terminals and resource usage through the API and leaves the rest
// (create, compose, volume helpers, ...) to the CLI, which it also falls
// back to while the socket cannot be used. NewStater does the same for the
// resource usage alone.
package dockerapi

import (
	"bufio"
	"bytes"
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

	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/runtime/cli"
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

// Kind classifies the answer as one of the runtime's sentinel errors (nil
// if none fits): by status code, else by the message, which is the same
// the CLI prints.
func (e *Error) Kind() error {
	switch e.Status {
	case http.StatusNotFound:
		return runtime.ErrNotFound
	case http.StatusConflict:
		return runtime.ErrConflict
	case http.StatusBadRequest:
		if k := cli.ErrorKind(e.Message); k != nil {
			return k
		}
		return runtime.ErrInvalid
	case http.StatusNotImplemented:
		return runtime.ErrUnsupported
	}
	return cli.ErrorKind(e.Message)
}

// newError reads the daemon's {"message": ...} error document.
func newError(path string, status int, body []byte) *Error {
	var e struct{ Message string }
	if json.Unmarshal(body, &e) != nil || e.Message == "" {
		e.Message = strings.TrimSpace(string(body))
	}
	return &Error{Path: path, Status: status, Message: e.Message}
}

// IsNotFound reports whether err is a 404 of the daemon (e.g. a container
// removed between listing and asking for its stats).
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// transportError means the daemon did not answer: the socket could not be
// reached (Dial) or the connection broke before a response arrived.
type transportError struct {
	path string
	err  error
	dial bool // the connection was never made: the request did not leave
}

func (e *transportError) Error() string { return fmt.Sprintf("docker API %s: %v", e.path, e.err) }
func (e *transportError) Unwrap() error { return e.err }

func newTransportError(path string, err error) *transportError {
	var op *net.OpError
	return &transportError{path: path, err: err, dial: errors.As(err, &op) && op.Op == "dial"}
}

// isTransport reports whether err means the daemon did not answer.
func isTransport(err error) bool {
	var t *transportError
	return errors.As(err, &t)
}

// isDial reports whether err means the request never reached the daemon.
func isDial(err error) bool {
	var t *transportError
	return errors.As(err, &t) && t.dial
}

// request builds a request with an optional JSON body.
func request(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// send performs req and returns the response if it is a 2xx answer.
func (c *Client) send(req *http.Request) (*http.Response, error) {
	path := req.URL.RequestURI()
	resp, err := c.http.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, fmt.Errorf("docker API %s: %w", path, req.Context().Err())
		}
		return nil, newTransportError(path, err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, newError(path, resp.StatusCode, data)
	}
	return resp, nil
}

// get requests path and decodes the JSON answer into v.
func (c *Client) get(ctx context.Context, path string, v any) error {
	return c.do(ctx, http.MethodGet, path, nil, v)
}

// do sends a request with an optional JSON body and decodes the JSON answer
// into v (skipped if v is nil).
func (c *Client) do(ctx context.Context, method, path string, body, v any) error {
	req, err := request(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.send(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if v == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(v); err != nil {
		return fmt.Errorf("docker API %s: decode: %w", path, err)
	}
	return nil
}

// raw requests path and returns the undecoded JSON answer.
func (c *Client) raw(ctx context.Context, path string) (json.RawMessage, error) {
	var v json.RawMessage
	if err := c.get(ctx, path, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// stream sends a request and returns the body of the answer unread (logs,
// pull progress). It ends when ctx is done.
func (c *Client) stream(ctx context.Context, method, path string, body any, header http.Header) (io.ReadCloser, error) {
	req, err := request(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.send(req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// hijack sends a request that turns the connection into a raw
// bidirectional stream (exec start, attach) and returns it. Reads must go
// through the returned reader, which may hold bytes already received.
// ctx only bounds the handshake.
func (c *Client) hijack(ctx context.Context, method, path string, body any) (net.Conn, *bufio.Reader, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, nil, newTransportError(path, err)
	}
	req, err := request(context.Background(), method, path, body)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	fail := func(err error) (net.Conn, *bufio.Reader, error) {
		conn.Close()
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("docker API %s: %w", path, ctx.Err())
		}
		return nil, nil, newTransportError(path, err)
	}
	if err := req.Write(conn); err != nil {
		stop()
		return fail(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if !stop() || err != nil {
		if err == nil {
			err = ctx.Err()
		}
		return fail(err)
	}
	_ = conn.SetDeadline(time.Time{})
	switch resp.StatusCode {
	case http.StatusSwitchingProtocols, http.StatusOK:
		return conn, br, nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	conn.Close()
	return nil, nil, newError(path, resp.StatusCode, data)
}

// Ping checks that the daemon answers on the socket.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		return err
	}
	resp, err := c.send(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return nil
}

// Close drops idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }
