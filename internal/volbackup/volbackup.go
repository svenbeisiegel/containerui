// Package volbackup writes and reads volume backup files: one tar stream,
// zstd-compressed or plain, that holds a manifest describing the volumes
// (driver, options, labels) and the content of each volume.
//
//	manifest.json            the first entry
//	volumes/<name>/<path>    the content of volume <name>, one volume after the other
//
// Ownership is kept as numeric uid/gid (user and group names are dropped),
// so a restore does not depend on the names known to the helper image.
package volbackup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	// Format identifies a volume backup manifest.
	Format = "containerui-volume-backup"
	// Version is the manifest version this package writes and the newest
	// it reads.
	Version = 1
	// ManifestName is the name of the manifest entry.
	ManifestName = "manifest.json"

	volumesDir  = "volumes/"
	maxManifest = 1 << 20
)

// ErrInvalid is wrapped by the errors about a file that is no volume backup
// or a damaged one.
var ErrInvalid = errors.New("invalid volume backup")

// Compression is how the tar stream is compressed.
type Compression string

const (
	Zstd Compression = "zstd"
	None Compression = "none"
)

// ParseCompression accepts "zstd" and "none"; "" is zstd.
func ParseCompression(s string) (Compression, error) {
	switch Compression(s) {
	case "", Zstd:
		return Zstd, nil
	case None:
		return None, nil
	}
	return "", fmt.Errorf("unknown compression %q", s)
}

// Ext is the file name extension for the compression.
func (c Compression) Ext() string {
	if c == None {
		return ".tar"
	}
	return ".tar.zst"
}

// ContentType is the media type of a backup file with the compression.
func (c Compression) ContentType() string {
	if c == None {
		return "application/x-tar"
	}
	return "application/zstd"
}

// Manifest describes a backup file.
type Manifest struct {
	Format      string      `json:"format"`
	Version     int         `json:"version"`
	Created     time.Time   `json:"created"`
	Creator     string      `json:"creator,omitempty"` // e.g. "ContainerUI 1.4.0"
	Host        string      `json:"host,omitempty"`
	Runtime     string      `json:"runtime,omitempty"` // e.g. "docker"
	Compression Compression `json:"compression"`
	Volumes     []Volume    `json:"volumes"`
}

// Volume describes how a backed-up volume was created.
type Volume struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver,omitempty"`
	Scope      string            `json:"scope,omitempty"`
	Options    map[string]string `json:"options,omitempty"` // driver options (--opt)
	Labels     map[string]string `json:"labels,omitempty"`
	Mountpoint string            `json:"mountpoint,omitempty"`
	UsedBy     []string          `json:"usedBy,omitempty"` // containers that mounted it
}

// Volume returns the manifest entry of the named volume.
func (m Manifest) Volume(name string) (Volume, bool) {
	for _, v := range m.Volumes {
		if v.Name == name {
			return v, true
		}
	}
	return Volume{}, false
}

// Stats counts the entries and the file bytes of a volume.
type Stats struct {
	Entries int   // files, directories, links
	Bytes   int64 // size of the regular files
}

// EntriesText is the number of entries in words ("1 entry", "3 entries").
func (s Stats) EntriesText() string {
	if s.Entries == 1 {
		return "1 entry"
	}
	return fmt.Sprintf("%d entries", s.Entries)
}

func (s *Stats) add(h *tar.Header) {
	s.Entries++
	if h.Typeflag == tar.TypeReg {
		s.Bytes += h.Size
	}
}

// ExportFunc writes the content of a volume as an uncompressed tar stream.
type ExportFunc func(ctx context.Context, name string, w io.Writer) error

// ImportFunc extracts an uncompressed tar stream into a volume; with
// replace the volume is emptied first.
type ImportFunc func(ctx context.Context, name string, r io.Reader, replace bool) error

func (m *Manifest) validate() error {
	if m.Format != Format {
		return fmt.Errorf("%w: not a ContainerUI volume backup", ErrInvalid)
	}
	if m.Version < 1 || m.Version > Version {
		return fmt.Errorf("%w: manifest version %d is not supported (newest: %d)", ErrInvalid, m.Version, Version)
	}
	if len(m.Volumes) == 0 {
		return fmt.Errorf("%w: no volumes", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, v := range m.Volumes {
		if !validName(v.Name) {
			return fmt.Errorf("%w: invalid volume name %q", ErrInvalid, v.Name)
		}
		if seen[v.Name] {
			return fmt.Errorf("%w: volume %q is listed twice", ErrInvalid, v.Name)
		}
		seen[v.Name] = true
	}
	return nil
}

// validName accepts what can be a path element: volume names are checked
// by the runtime, this only keeps the archive layout intact.
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

// cleanRel turns an entry name of a volume's tar stream ("./a/b", "a/b/",
// ".") into a clean relative path, "" for the volume's root. Names that
// leave the volume are rejected.
func cleanRel(name string) (string, bool) {
	if strings.HasPrefix(name, "/") || strings.Contains(name, "\x00") {
		return "", false
	}
	p := path.Clean(name)
	if p == "." {
		return "", true
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}

// archiveName is the name of a volume entry in the backup file.
func archiveName(volume, rel string, dir bool) string {
	n := volumesDir + volume + "/" + rel
	if dir && rel != "" {
		n += "/"
	}
	return n
}

// splitName splits a backup entry name into the volume and the clean path
// within it.
func splitName(name string) (volume, rel string, ok bool) {
	rest, found := strings.CutPrefix(name, volumesDir)
	if !found {
		return "", "", false
	}
	volume, rel, _ = strings.Cut(rest, "/")
	if !validName(volume) {
		return "", "", false
	}
	rel, ok = cleanRel(rel)
	return volume, rel, ok
}

// rewrite prepares a header read from one tar stream to be written to
// another under name: the format is chosen anew (the name may be longer),
// user and group names are dropped and PAX records the header fields
// replace are removed. Extended attributes are kept.
func rewrite(h *tar.Header, name string) {
	h.Name = name
	h.Uname, h.Gname = "", ""
	h.Format = tar.FormatUnknown
	for _, k := range []string{"path", "linkpath", "uname", "gname", "uid", "gid", "size", "mtime", "atime", "ctime"} {
		delete(h.PAXRecords, k)
	}
	h.AccessTime, h.ChangeTime = time.Time{}, time.Time{}
}

// Write writes a backup file with the volumes of m to w, compressed with
// m.Compression; export provides each volume's content. It returns the
// statistics per volume.
func Write(ctx context.Context, w io.Writer, m Manifest, export ExportFunc) (map[string]Stats, error) {
	m.Format, m.Version = Format, Version
	if m.Compression == "" {
		m.Compression = Zstd
	}
	if m.Created.IsZero() {
		m.Created = time.Now().UTC()
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	out := w
	var enc *zstd.Encoder
	switch m.Compression {
	case Zstd:
		var err error
		if enc, err = zstd.NewWriter(w); err != nil {
			return nil, err
		}
		out = enc
	case None:
	default:
		return nil, fmt.Errorf("unknown compression %q", m.Compression)
	}
	stats, err := writeTar(ctx, tar.NewWriter(out), m, export)
	if enc != nil {
		if cerr := enc.Close(); err == nil {
			err = cerr
		}
	}
	return stats, err
}

func writeTar(ctx context.Context, tw *tar.Writer, m Manifest, export ExportFunc) (map[string]Stats, error) {
	doc, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	doc = append(doc, '\n')
	if err := tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o644, Size: int64(len(doc)), ModTime: m.Created, Typeflag: tar.TypeReg}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(doc); err != nil {
		return nil, err
	}
	stats := map[string]Stats{}
	for _, v := range m.Volumes {
		s, err := copyVolume(ctx, tw, v.Name, export)
		stats[v.Name] = s
		if err != nil {
			return stats, fmt.Errorf("volume %s: %w", v.Name, err)
		}
	}
	return stats, tw.Close()
}

// copyVolume copies the tar stream export writes for a volume into tw,
// with the entry names moved below the volume's folder.
func copyVolume(ctx context.Context, tw *tar.Writer, name string, export ExportFunc) (Stats, error) {
	var s Stats
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		err := export(ctx, name, pw)
		pw.CloseWithError(err)
		errc <- err
	}()
	fail := func(err error) (Stats, error) {
		cancel()
		pr.CloseWithError(err)
		if xerr := <-errc; xerr != nil && !errors.Is(xerr, io.ErrClosedPipe) && !errors.Is(xerr, context.Canceled) {
			return s, xerr // the exporter's error explains a broken stream best
		}
		return s, err
	}
	root := false
	tr := tar.NewReader(pr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(err)
		}
		rel, ok := cleanRel(h.Name)
		if !ok {
			return fail(fmt.Errorf("unsafe path %q in the volume's content", h.Name))
		}
		if rel == "" {
			if h.Typeflag != tar.TypeDir {
				return fail(fmt.Errorf("the volume's root %q is not a directory", h.Name))
			}
			root = true
		}
		if h.Typeflag == tar.TypeLink {
			target, ok := cleanRel(h.Linkname)
			if !ok || target == "" {
				return fail(fmt.Errorf("unsafe hard link %q -> %q", h.Name, h.Linkname))
			}
			h.Linkname = archiveName(name, target, false)
		}
		rewrite(h, archiveName(name, rel, h.Typeflag == tar.TypeDir))
		if err := tw.WriteHeader(h); err != nil {
			return fail(err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return fail(err)
		}
		s.add(h)
	}
	// The tar end marker may be followed by padding.
	if _, err := io.Copy(io.Discard, pr); err != nil {
		return fail(err)
	}
	if err := <-errc; err != nil {
		return s, err
	}
	if !root {
		// Keep an empty volume in the file, and give restore a place to
		// start even if the exporter left out the root.
		if err := tw.WriteHeader(&tar.Header{Name: archiveName(name, "", true), Mode: 0o755, Typeflag: tar.TypeDir, ModTime: time.Now()}); err != nil {
			return s, err
		}
		s.Entries++
	}
	return s, nil
}

// Reader reads a backup file. Use Open, then Manifest is set; Next
// walks the entries.
type Reader struct {
	Manifest    Manifest
	Compression Compression

	tr  *tar.Reader
	dec *zstd.Decoder
}

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// Open detects the compression of a backup file and reads its manifest.
// Close releases the decompressor.
func Open(r io.Reader) (*Reader, error) {
	br := bufio.NewReader(r)
	rd := &Reader{Compression: None}
	src := io.Reader(br)
	if magic, _ := br.Peek(4); bytes.Equal(magic, zstdMagic) {
		dec, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, err
		}
		rd.dec, rd.Compression, src = dec, Zstd, dec
	}
	rd.tr = tar.NewReader(src)
	h, err := rd.tr.Next()
	if err != nil {
		rd.Close()
		return nil, fmt.Errorf("%w: %v", ErrInvalid, readErr(err))
	}
	if h.Name != ManifestName || h.Typeflag != tar.TypeReg {
		rd.Close()
		return nil, fmt.Errorf("%w: the file does not start with %s", ErrInvalid, ManifestName)
	}
	if h.Size > maxManifest {
		rd.Close()
		return nil, fmt.Errorf("%w: %s is too large", ErrInvalid, ManifestName)
	}
	if err := json.NewDecoder(rd.tr).Decode(&rd.Manifest); err != nil {
		rd.Close()
		return nil, fmt.Errorf("%w: %s: %v", ErrInvalid, ManifestName, err)
	}
	if err := rd.Manifest.validate(); err != nil {
		rd.Close()
		return nil, err
	}
	return rd, nil
}

func readErr(err error) error {
	if err == io.EOF {
		return errors.New("the file is empty or not a tar archive")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.New("the file is truncated")
	}
	return err
}

// Close releases the decompressor.
func (rd *Reader) Close() {
	if rd.dec != nil {
		rd.dec.Close()
	}
}

// entry is a volume entry of a backup file.
type entry struct {
	h      *tar.Header
	volume string
	rel    string
}

// next returns the next volume entry, io.EOF at the end.
func (rd *Reader) next() (entry, error) {
	h, err := rd.tr.Next()
	if err == io.EOF {
		return entry{}, io.EOF
	}
	if err != nil {
		return entry{}, fmt.Errorf("%w: %v", ErrInvalid, readErr(err))
	}
	v, rel, ok := splitName(h.Name)
	if !ok {
		return entry{}, fmt.Errorf("%w: unexpected entry %q", ErrInvalid, h.Name)
	}
	if _, listed := rd.Manifest.Volume(v); !listed {
		return entry{}, fmt.Errorf("%w: entry %q belongs to no volume of the manifest", ErrInvalid, h.Name)
	}
	if h.Typeflag == tar.TypeLink {
		lv, lrel, ok := splitName(h.Linkname)
		if !ok || lv != v || lrel == "" {
			return entry{}, fmt.Errorf("%w: hard link %q -> %q leaves its volume", ErrInvalid, h.Name, h.Linkname)
		}
	}
	return entry{h: h, volume: v, rel: rel}, nil
}

// Scan reads a whole backup file and checks it: the manifest, that every
// entry belongs to a volume of the manifest, that each volume's entries
// are in one piece, and that the file is complete. It returns the
// statistics per volume.
func Scan(r io.Reader) (Manifest, map[string]Stats, error) {
	rd, err := Open(r)
	if err != nil {
		return Manifest{}, nil, err
	}
	defer rd.Close()
	stats := map[string]Stats{}
	done := map[string]bool{}
	cur := ""
	for {
		e, err := rd.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return rd.Manifest, stats, err
		}
		if e.volume != cur {
			if done[e.volume] {
				return rd.Manifest, stats, fmt.Errorf("%w: the content of volume %s is split", ErrInvalid, e.volume)
			}
			done[cur], cur = true, e.volume
		}
		s := stats[e.volume]
		s.add(e.h)
		stats[e.volume] = s
		// Read the data, so a damaged compressed stream is noticed.
		if _, err := io.Copy(io.Discard, rd.tr); err != nil {
			return rd.Manifest, stats, fmt.Errorf("%w: %v", ErrInvalid, readErr(err))
		}
	}
	for _, v := range rd.Manifest.Volumes {
		if _, ok := stats[v.Name]; !ok {
			return rd.Manifest, stats, fmt.Errorf("%w: no content for volume %s", ErrInvalid, v.Name)
		}
	}
	return rd.Manifest, stats, nil
}

// Target says where a volume of a backup file is restored to.
type Target struct {
	Name    string // the volume to extract into; it must exist
	Replace bool   // empty the volume first
}

// Restore extracts the volumes of a backup file that targets names (by
// their name in the file) with importFn, one after the other in a single
// pass; the others are skipped. progress, if set, is told about each
// restored volume. It returns the statistics of the restored volumes.
func Restore(ctx context.Context, r io.Reader, targets map[string]Target, importFn ImportFunc, progress func(string)) (map[string]Stats, error) {
	rd, err := Open(r)
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	for src := range targets {
		if _, ok := rd.Manifest.Volume(src); !ok {
			return nil, fmt.Errorf("volume %s is not in the backup file", src)
		}
	}
	stats := map[string]Stats{}
	var cur *volumeImport
	finish := func() error {
		if cur == nil {
			return nil
		}
		c := cur
		cur = nil
		if err := c.close(); err != nil {
			return fmt.Errorf("volume %s: %w", c.target.Name, err)
		}
		stats[c.source] = c.stats
		if progress != nil {
			progress(fmt.Sprintf("Restored volume %s into %s: %s, %s.", c.source, c.target.Name, c.stats.EntriesText(), FormatBytes(c.stats.Bytes)))
		}
		return nil
	}
	curSource := ""
	for {
		e, err := rd.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if cur != nil {
				cur.abort(err)
			}
			return stats, err
		}
		if e.volume != curSource {
			if err := finish(); err != nil {
				return stats, err
			}
			curSource = e.volume
			if t, ok := targets[e.volume]; ok {
				if _, again := stats[e.volume]; again {
					return stats, fmt.Errorf("%w: the content of volume %s is split", ErrInvalid, e.volume)
				}
				cur = startImport(ctx, e.volume, t, importFn)
			}
		}
		if cur == nil {
			continue // a skipped volume; Next discards the data
		}
		if err := cur.write(e, rd.tr); err != nil {
			return stats, fmt.Errorf("volume %s: %w", cur.target.Name, err)
		}
	}
	if err := finish(); err != nil {
		return stats, err
	}
	for src := range targets {
		if _, ok := stats[src]; !ok {
			return stats, fmt.Errorf("%w: no content for volume %s", ErrInvalid, src)
		}
	}
	return stats, nil
}

// volumeImport feeds one volume's entries to a running ImportFunc.
type volumeImport struct {
	source string
	target Target
	stats  Stats
	cancel context.CancelFunc
	pw     *io.PipeWriter
	tw     *tar.Writer
	errc   chan error
}

func startImport(ctx context.Context, source string, t Target, importFn ImportFunc) *volumeImport {
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	vi := &volumeImport{source: source, target: t, cancel: cancel, pw: pw, tw: tar.NewWriter(pw), errc: make(chan error, 1)}
	go func() {
		err := importFn(ctx, t.Name, pr, t.Replace)
		if err == nil {
			// Unblock the writer if the importer stopped reading early.
			_, err = io.Copy(io.Discard, pr)
		}
		pr.CloseWithError(errOr(err, io.ErrClosedPipe))
		vi.errc <- err
	}()
	return vi
}

func errOr(err, def error) error {
	if err != nil {
		return err
	}
	return def
}

func (vi *volumeImport) write(e entry, data io.Reader) error {
	h := e.h
	name := "./" + e.rel
	if e.rel == "" {
		name = "./"
	} else if h.Typeflag == tar.TypeDir {
		name += "/"
	}
	if h.Typeflag == tar.TypeLink {
		_, lrel, _ := splitName(h.Linkname)
		h.Linkname = "./" + lrel
	}
	rewrite(h, name)
	err := vi.tw.WriteHeader(h)
	if err == nil {
		_, err = io.Copy(vi.tw, data)
	}
	if err != nil {
		return vi.abort(err)
	}
	vi.stats.add(h)
	return nil
}

// abort stops the importer and returns the error that explains the
// failure best: the importer's own, if it failed.
func (vi *volumeImport) abort(err error) error {
	vi.cancel()
	vi.pw.CloseWithError(err)
	if ierr := <-vi.errc; ierr != nil && !errors.Is(ierr, context.Canceled) {
		return ierr
	}
	return err
}

func (vi *volumeImport) close() error {
	defer vi.cancel()
	err := vi.tw.Close()
	if err == nil {
		err = vi.pw.Close()
	}
	if err != nil {
		return vi.abort(err)
	}
	return <-vi.errc
}

// FormatBytes formats a byte count for people (1.5 MiB).
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
