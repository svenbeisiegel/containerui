package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// wslc prints timestamps like "2026-10-05 08:37:42 +0200 CEST" (containers,
// images) or "2026-10-05 06:34:22.488226783 +0000 UTC" (networks).
const timeLayout = "2006-01-02 15:04:05.999999999 -0700 MST"

func parseTime(s string) time.Time {
	t, _ := time.Parse(timeLayout, s)
	return t
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normalize strips a BOM and converts UTF-16LE output to UTF-8. wslc
// currently writes UTF-8, but wsl.exe writes UTF-16, so be defensive.
func normalize(b []byte) []byte {
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		return decodeUTF16LE(b[2:])
	}
	if len(b) >= 2 && b[1] == 0 && bytes.Count(b, []byte{0}) >= len(b)/3 {
		return decodeUTF16LE(b)
	}
	return bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF"))
}

func decodeUTF16LE(b []byte) []byte {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return []byte(string(utf16.Decode(u)))
}

// decodeNDJSON decodes newline-delimited JSON objects into a slice of T.
// It also accepts a single JSON array (podman, or a future wslc).
func decodeNDJSON[T any](data []byte) ([]T, error) {
	data = bytes.TrimSpace(normalize(data))
	out := []T{}
	if len(data) == 0 {
		return out, nil
	}
	if data[0] == '[' {
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("decode CLI output: %w", err)
		}
		return out, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var v T
		if err := dec.Decode(&v); errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return nil, fmt.Errorf("decode CLI output: %w", err)
		}
		out = append(out, v)
	}
}

type containerJSON struct {
	ID         string `json:"ID"`
	Names      string `json:"Names"`
	Image      string `json:"Image"`
	Command    string `json:"Command"`
	State      string `json:"State"`
	Status     string `json:"Status"`
	Ports      string `json:"Ports"`
	Networks   string `json:"Networks"`
	Mounts     string `json:"Mounts"`
	CreatedAt  string `json:"CreatedAt"`
	RunningFor string `json:"RunningFor"`
	Labels     string `json:"Labels"`
}

func parseContainers(data []byte) ([]runtime.Container, error) {
	rows, err := decodeNDJSON[containerJSON](data)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.Container, 0, len(rows))
	for _, r := range rows {
		out = append(out, runtime.Container{
			ID:         r.ID,
			Name:       strings.TrimPrefix(r.Names, "/"),
			Image:      r.Image,
			Command:    strings.Trim(r.Command, `"`),
			State:      runtime.ParseState(strings.ToLower(r.State)),
			Status:     r.Status,
			Ports:      r.Ports,
			Networks:   splitList(r.Networks),
			Mounts:     splitList(r.Mounts),
			Created:    parseTime(r.CreatedAt),
			RunningFor: r.RunningFor,
			Labels:     parseLabels(r.Labels),
		})
	}
	return out, nil
}

type imageJSON struct {
	ID         string `json:"ID"`
	Repository string `json:"Repository"`
	Tag        string `json:"Tag"`
	Digest     string `json:"Digest"`
	Size       string `json:"Size"`
	CreatedAt  string `json:"CreatedAt"`
	Containers string `json:"Containers"`
}

func parseImages(data []byte) ([]runtime.Image, error) {
	rows, err := decodeNDJSON[imageJSON](data)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.Image, 0, len(rows))
	for _, r := range rows {
		n, _ := strconv.Atoi(r.Containers)
		digest := r.Digest
		if digest == "<none>" {
			digest = ""
		}
		out = append(out, runtime.Image{
			ID:         r.ID,
			Repository: r.Repository,
			Tag:        r.Tag,
			Digest:     digest,
			Size:       r.Size,
			Created:    parseTime(r.CreatedAt),
			Containers: n,
		})
	}
	return out, nil
}

type volumeJSON struct {
	Name       string     `json:"Name"`
	Driver     string     `json:"Driver"`
	Mountpoint string     `json:"Mountpoint"`
	Scope      string     `json:"Scope"`
	Labels     labelsJSON `json:"Labels"`
}

func parseVolumes(data []byte) ([]runtime.Volume, error) {
	rows, err := decodeNDJSON[volumeJSON](data)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.Volume, 0, len(rows))
	for _, r := range rows {
		out = append(out, runtime.Volume{Name: r.Name, Driver: r.Driver, Mountpoint: r.Mountpoint, Scope: r.Scope, Labels: r.Labels})
	}
	return out, nil
}

type networkJSON struct {
	ID        string `json:"ID"`
	Name      string `json:"Name"`
	Driver    string `json:"Driver"`
	Scope     string `json:"Scope"`
	Internal  string `json:"Internal"`
	IPv6      string `json:"IPv6"`
	CreatedAt string `json:"CreatedAt"`
	Labels    string `json:"Labels"`
}

func parseNetworks(data []byte) ([]runtime.Network, error) {
	rows, err := decodeNDJSON[networkJSON](data)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.Network, 0, len(rows))
	for _, r := range rows {
		out = append(out, runtime.Network{
			ID:       r.ID,
			Name:     r.Name,
			Driver:   r.Driver,
			Scope:    r.Scope,
			Internal: r.Internal == "true",
			IPv6:     r.IPv6 == "true",
			Created:  parseTime(r.CreatedAt),
			Labels:   parseLabels(r.Labels),
		})
	}
	return out, nil
}

// parseLabels splits the "k1=v1,k2=v2" label summary of docker and wslc list
// output. Values may contain commas (wslc stores JSON metadata in a label),
// so a comma only separates labels when what follows looks like "key=".
func parseLabels(s string) map[string]string {
	if s == "" {
		return nil
	}
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' && startsLabel(s[i+1:]) {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	out := make(map[string]string, len(parts))
	for _, p := range parts {
		k, v, _ := strings.Cut(p, "=")
		if k = strings.TrimSpace(k); k != "" {
			out[k] = v
		}
	}
	return out
}

// startsLabel reports whether s begins with a label key followed by '='.
func startsLabel(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '=':
			return i > 0
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-', c == '/':
		default:
			return false
		}
	}
	return false
}

// labelsJSON accepts labels as docker's "k=v,..." string or as an object
// (podman, docker volume ls on some versions).
type labelsJSON map[string]string

func (l *labelsJSON) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*l = parseLabels(s)
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*l = m
	return nil
}

// wslc's human-readable messages are localized; only the error code that
// follows them ("Fehlercode: WSLC_E_IMAGE_NOT_FOUND") is stable.
var errorCodeRe = regexp.MustCompile(`\b((?:WSLC_E|ERROR|E)_[A-Z0-9_]+)\b`)

func errorKind(code string) error {
	switch {
	case code == "":
		return nil
	case strings.HasSuffix(code, "_NOT_FOUND"), code == "ERROR_FILE_NOT_FOUND", code == "ERROR_PATH_NOT_FOUND":
		return runtime.ErrNotFound
	case code == "WSLC_E_CONTAINER_IS_RUNNING", code == "ERROR_SHARING_VIOLATION",
		code == "ERROR_ALREADY_EXISTS", strings.HasSuffix(code, "_IN_USE"), strings.Contains(code, "CONFLICT"):
		return runtime.ErrConflict
	case code == "ERROR_NOT_SUPPORTED", code == "E_NOTIMPL":
		return runtime.ErrUnsupported
	case code == "E_INVALIDARG", code == "ERROR_INVALID_PARAMETER":
		return runtime.ErrInvalid
	}
	return nil
}

// commandError builds a runtime.CommandError from a failed CLI call.
func (d *Driver) commandError(op string, exitCode int, stdout, stderr []byte) *runtime.CommandError {
	text := string(normalize(stderr))
	if strings.TrimSpace(text) == "" {
		text = string(normalize(stdout))
	}
	code := ""
	if m := errorCodeRe.FindStringSubmatch(text); m != nil {
		code = m[1]
	}
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || (code != "" && strings.Contains(l, code)) || strings.Contains(l, "github.com/microsoft/WSL/issues") {
			continue
		}
		lines = append(lines, l)
	}
	kind := errorKind(code)
	if kind == nil && d.flavor.textKind != nil {
		kind = d.flavor.textKind(text)
	}
	return &runtime.CommandError{
		Op:       op,
		Code:     code,
		ExitCode: exitCode,
		Output:   strings.Join(lines, " "),
		Kind:     kind,
	}
}
