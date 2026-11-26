package dockerapi

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/svenbeisiegel/containerui/internal/registry"
	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/runtime/cli"
)

// errWrite marks a failed write to the destination of a stream, so it is
// not taken for a broken stream.
type errWrite struct{ err error }

func (e errWrite) Error() string { return e.err.Error() }
func (e errWrite) Unwrap() error { return e.err }

// demux copies a multiplexed stream (the output of a container or exec
// without a TTY: frames of an 8-byte header and a payload) to stdout and
// stderr. An error frame (stream 3) ends it with that error.
func demux(r io.Reader, stdout, stderr io.Writer) error {
	var hdr [8]byte
	buf := make([]byte, 32<<10)
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		n := int64(binary.BigEndian.Uint32(hdr[4:]))
		var w io.Writer
		switch hdr[0] {
		case 0, 1:
			w = stdout
		case 2:
			w = stderr
		case 3:
			msg, _ := io.ReadAll(io.LimitReader(r, min(n, 64<<10)))
			return errors.New(strings.TrimSpace(string(msg)))
		default:
			return fmt.Errorf("unexpected stream type %d", hdr[0])
		}
		for n > 0 {
			m, err := r.Read(buf[:min(n, int64(len(buf)))])
			if m > 0 {
				n -= int64(m)
				if _, werr := w.Write(buf[:m]); werr != nil {
					return errWrite{werr}
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) && n > 0 {
					return io.ErrUnexpectedEOF
				}
				if n > 0 {
					return err
				}
			}
		}
	}
}

// containerTTY reports whether the container has a TTY (its output is then
// not multiplexed).
func (d *Driver) containerTTY(ctx context.Context, id string) (bool, error) {
	var c struct{ Config struct{ Tty bool } }
	if err := d.api.get(ctx, "/containers/"+esc(id)+"/json", &c); err != nil {
		return false, err
	}
	return c.Config.Tty, nil
}

func (d *Driver) ContainerLogs(ctx context.Context, id string, opts runtime.LogsOpts, w io.Writer) error {
	if err := runtime.ValidateContainerID(id); err != nil {
		return err
	}
	// Only errors before the first byte (no answer at all) go to the CLI,
	// so nothing is written twice.
	_, err := try(d.fb, ctx, onTransport, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, d.apiLogs(ctx, id, opts, w)
	}, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, d.Driver.ContainerLogs(ctx, id, opts, w)
	})
	return err
}

func (d *Driver) apiLogs(ctx context.Context, id string, opts runtime.LogsOpts, w io.Writer) error {
	const op = "container logs"
	tty, err := d.containerTTY(ctx, id)
	if err != nil {
		return opError(op, err)
	}
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}}
	if opts.Follow {
		q.Set("follow", "1")
	}
	if opts.Tail > 0 {
		q.Set("tail", strconv.Itoa(opts.Tail))
	}
	body, err := d.api.stream(ctx, http.MethodGet, "/containers/"+esc(id)+"/logs?"+q.Encode(), nil, nil)
	if err != nil {
		return opError(op, err)
	}
	defer body.Close()
	if tty {
		_, err = io.Copy(w, body)
	} else {
		err = demux(body, w, w)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s %s: %w", d.Name(), op, ctx.Err())
	}
	if err != nil {
		var we errWrite
		if errors.As(err, &we) {
			return we.err
		}
		return fmt.Errorf("%s %s: %w", d.Name(), op, err)
	}
	return nil
}

func (d *Driver) PullImage(ctx context.Context, ref string, progress func(string)) error {
	if err := runtime.ValidateImageRef(ref); err != nil {
		return err
	}
	if progress == nil {
		progress = func(string) {}
	}
	header, ok := d.registryAuth(ref)
	if !ok {
		// The login is kept by a credential helper: the CLI asks it.
		return d.Driver.PullImage(ctx, ref, progress)
	}
	ctx, cancel := context.WithTimeout(ctx, d.pullTimeout)
	defer cancel()
	_, err := try(d.fb, ctx, onDial, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, d.apiPull(ctx, ref, header, progress)
	}, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, d.Driver.PullImage(ctx, ref, progress)
	})
	return err
}

// registryAuth returns the X-Registry-Auth header for pulling ref (nil
// for anonymous access). ok is false if the API cannot pull it with the
// login it needs.
func (d *Driver) registryAuth(ref string) (header http.Header, ok bool) {
	if d.creds == nil {
		return nil, true
	}
	r, err := registry.ParseRef(ref)
	if err != nil {
		return nil, true // not a registry reference: the daemon answers
	}
	user, pass, err := d.creds(r.Registry)
	if err != nil {
		return nil, false
	}
	if user == "" {
		return nil, true
	}
	b, _ := json.Marshal(map[string]string{"username": user, "password": pass, "serveraddress": r.Registry})
	return http.Header{"X-Registry-Auth": {base64.URLEncoding.EncodeToString(b)}}, true
}

// pullQuery splits ref into the fromImage and tag parameters of
// /images/create; tag is the digest of a digest reference.
func pullQuery(ref string) url.Values {
	repo, digest, hasDigest := strings.Cut(ref, "@")
	tag := digest
	if !hasDigest {
		repo, tag = runtime.SplitRef(ref)
	} else if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i] // repo:tag@digest pulls the digest
	}
	return url.Values{"fromImage": {repo}, "tag": {tag}}
}

// pullMessage is one line of the JSON progress stream of a pull.
type pullMessage struct {
	Status      string `json:"status"`
	ID          string `json:"id"`
	Progress    string `json:"progress"`
	Error       string `json:"error"`
	ErrorDetail *struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

func (d *Driver) apiPull(ctx context.Context, ref string, header http.Header, progress func(string)) error {
	const op = "image pull"
	body, err := d.api.stream(ctx, http.MethodPost, "/images/create?"+pullQuery(ref).Encode(), nil, header)
	if err != nil {
		return opError(op, err)
	}
	defer body.Close()
	dec := json.NewDecoder(bufio.NewReader(body))
	for {
		var m pullMessage
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("%s %s: %w", d.Name(), op, ctx.Err())
			}
			return fmt.Errorf("%s %s: %w", d.Name(), op, err)
		}
		if msg := m.Error; msg != "" || m.ErrorDetail != nil {
			if msg == "" {
				msg = m.ErrorDetail.Message
			}
			return &runtime.CommandError{Op: op, Output: msg, Kind: cli.ErrorKind(msg)}
		}
		// Like the CLI without a terminal: no progress bars, only the
		// steps ("abc123: Pull complete").
		if m.Progress != "" || m.Status == "" {
			continue
		}
		line := m.Status
		if m.ID != "" {
			line = m.ID + ": " + line
		}
		progress(line)
	}
}
