package hostlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// New returns the Windows Event Log, read through wevtutil.
func New() Source { return &eventLog{} }

// eventPoll is how often a followed channel is queried for new events.
var eventPoll = 2 * time.Second

// eventPollMax bounds the events read per poll.
const eventPollMax = 1000

type eventLog struct {
	mu    sync.Mutex
	chans []Option
}

func (e *eventLog) Kind() string          { return "eventlog" }
func (e *eventLog) Label() string         { return "Event Log" }
func (e *eventLog) SelectorLabel() string { return "Channel" }
func (e *eventLog) Priorities() []Option  { return eventPriorities }
func (e *eventLog) Ranges() []Option      { return eventRanges }

func (e *eventLog) Available() error {
	if _, err := exec.LookPath("wevtutil.exe"); err != nil {
		return errors.New("wevtutil.exe was not found")
	}
	return nil
}

func (e *eventLog) Selectors(ctx context.Context) ([]Option, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.chans != nil {
		return e.chans, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := wevtutil(ctx, &out, "el"); err != nil {
		return nil, fmt.Errorf("list event channels: %w", err)
	}
	e.chans = eventChannels(out.String())
	return e.chans, nil
}

func (e *eventLog) Read(ctx context.Context, q Query, w io.Writer) error {
	boot := sinceBoot()
	write := func(ev *event) error {
		_, err := io.WriteString(w, ev.line(time.Local)+"\n")
		return err
	}
	if !q.Follow {
		return e.query(ctx, q.Selector, eventXPath(q, boot, 0, 0), false, 0, write)
	}

	// The newest record of the channel, whatever the filter, is where
	// following starts; the tail is read up to it.
	var last uint64
	err := e.query(ctx, q.Selector, "*", true, 1, func(ev *event) error {
		last = ev.System.EventRecordID
		return nil
	})
	if err != nil {
		return err
	}
	if last > 0 {
		var tail []*event
		err := e.query(ctx, q.Selector, eventXPath(q, boot, 0, last), true, q.Tail, func(ev *event) error {
			tail = append(tail, ev)
			return nil
		})
		if err != nil {
			return err
		}
		slices.Reverse(tail)
		for _, ev := range tail {
			if err := write(ev); err != nil {
				return err
			}
		}
	}

	t := time.NewTicker(eventPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		// New events only; the time range applied to the start.
		nq := q
		nq.Since = ""
		err := e.query(ctx, q.Selector, eventXPath(nq, boot, last, 0), false, eventPollMax, func(ev *event) error {
			last = max(last, ev.System.EventRecordID)
			return write(ev)
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

func (e *eventLog) ExportExt() string { return ".evtx" }

// Export writes the selected events as an .evtx file.
func (e *eventLog) Export(ctx context.Context, q Query, w io.Writer) error {
	dir, err := os.MkdirTemp("", "containerui-evtx-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "export.evtx")
	boot := sinceBoot()
	if err := wevtutil(ctx, io.Discard, "epl", q.Selector, file, "/q:"+eventXPath(q, boot, 0, 0), "/ow:true"); err != nil {
		return err
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// query runs `wevtutil qe` and calls fn for each event; newest first when
// reverse is set, at most count events when count > 0.
func (e *eventLog) query(ctx context.Context, channel, xpath string, reverse bool, count int, fn func(*event) error) error {
	args := []string{"qe", channel, "/q:" + xpath, "/f:RenderedXml", "/e:Events", "/uni:true"}
	if reverse {
		args = append(args, "/rd:true")
	}
	if count > 0 {
		args = append(args, "/c:"+strconv.Itoa(count))
	}
	cmd := command(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	derr := decodeEvents(out, fn)
	_, _ = io.Copy(io.Discard, out)
	if err := commandErr(cmd.Wait(), &stderr); err != nil {
		return err
	}
	return derr
}

// wevtutil runs wevtutil with args, writing its output to w.
func wevtutil(ctx context.Context, w io.Writer, args ...string) error {
	cmd := command(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &stderr
	return commandErr(cmd.Run(), &stderr)
}

func command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "wevtutil.exe", args...)
	// No console window flashing up for each query.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func commandErr(err error, stderr *bytes.Buffer) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("wevtutil: %s", msg)
		}
		return fmt.Errorf("wevtutil exited with status %d", ee.ExitCode())
	}
	return err
}

var procGetTickCount64 = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetTickCount64")

// sinceBoot is the time since the system started.
func sinceBoot() time.Duration {
	ms, _, _ := procGetTickCount64.Call()
	return time.Duration(ms) * time.Millisecond
}
