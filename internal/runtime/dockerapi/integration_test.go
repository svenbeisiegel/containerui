//go:build integration

package dockerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/runtime/cli"
)

// TestParity compares what the API driver and the CLI driver return for
// the same daemon. It needs docker and/or podman with a reachable socket
// (podman: systemctl enable --now podman.socket) and only reads:
//
//	go test -tags integration ./internal/runtime/dockerapi/...
func TestParity(t *testing.T) {
	for _, f := range []*cli.Flavor{cli.Docker, cli.Podman} {
		t.Run(f.Name, func(t *testing.T) {
			c, err := cli.New(f, config.Runtime{})
			if err != nil {
				t.Skip(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if _, err := c.Info(ctx); err != nil {
				t.Skip("daemon not answering: ", err)
			}
			sock := c.APISocket(ctx)
			if sock == "" {
				t.Skip("no API socket")
			}
			a := Wrap(c, sock, Options{})
			if v, err := a.Info(ctx); err != nil {
				t.Fatal(err)
			} else {
				t.Logf("%s %s on %s", f.Name, v.Version, sock)
			}
			podman := f == cli.Podman

			cliCs, err := c.ListContainers(ctx, runtime.ListContainersOpts{All: true})
			if err != nil {
				t.Fatal(err)
			}
			apiCs, err := a.ListContainers(ctx, runtime.ListContainersOpts{All: true})
			if err != nil || a.Source() != "api" {
				t.Fatalf("API list: %v (source %q)", err, a.Source())
			}
			for _, cs := range [][]runtime.Container{cliCs, apiCs} {
				sort.Slice(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
				for i := range cs {
					// Rounded at different moments ("Up 59 seconds", "Up About a minute").
					cs[i].RunningFor = ""
					cs[i].Status = statusAge.ReplaceAllString(cs[i].Status, "<age>")
					sort.Strings(cs[i].Mounts) // the daemon lists them in no stable order
					if podman {
						// podman's CLI lists mount destinations and joins the
						// command differently; the API lists volume names like docker.
						cs[i].Mounts, cs[i].Command = nil, ""
					}
				}
			}
			compare(t, "containers", cliCs, apiCs)

			cliImgs, err := c.ListImages(ctx)
			if err != nil {
				t.Fatal(err)
			}
			apiImgs, err := a.ListImages(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, imgs := range [][]runtime.Image{cliImgs, apiImgs} {
				sort.Slice(imgs, func(i, j int) bool { return imgs[i].Ref()+imgs[i].ID < imgs[j].Ref()+imgs[j].ID })
				for i := range imgs {
					if imgs[i].Created.Unix() == 0 {
						// No date: the CLI prints 1970-01-01, the API driver leaves it zero.
						imgs[i].Created = time.Time{}
					}
					if podman {
						imgs[i].Digest = "" // the CLI reports one, the compat API does not
					}
				}
			}
			compare(t, "images", cliImgs, apiImgs)

			cliVols, err := c.ListVolumes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			apiVols, err := a.ListVolumes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, vs := range [][]runtime.Volume{cliVols, apiVols} {
				sort.Slice(vs, func(i, j int) bool { return vs[i].Name < vs[j].Name })
				for i := range vs {
					if len(vs[i].Options) == 0 {
						vs[i].Options = nil
					}
				}
			}
			compare(t, "volumes", cliVols, apiVols)

			cliNets, err := c.ListNetworks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			apiNets, err := a.ListNetworks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, ns := range [][]runtime.Network{cliNets, apiNets} {
				sort.Slice(ns, func(i, j int) bool { return ns[i].Name < ns[j].Name })
				for i := range ns {
					ns[i].Created = ns[i].Created.UTC().Truncate(time.Second)
				}
			}
			compare(t, "networks", cliNets, apiNets)

			if !podman && len(cliCs) > 0 {
				id := cliCs[0].ID
				x, err1 := c.InspectContainer(ctx, id)
				y, err2 := a.InspectContainer(ctx, id)
				if err1 != nil || err2 != nil {
					t.Fatal(err1, err2)
				}
				var vx, vy map[string]any
				_ = json.Unmarshal(x, &vx)
				_ = json.Unmarshal(y, &vy)
				compare(t, "inspect "+id, vx, vy)
			}
		})
	}
}

// statusAge is the time part of a container status.
var statusAge = regexp.MustCompile(`Less than a second|About an? \w+|\d+ \w+`)

func compare[T any](t *testing.T, what string, cliV, apiV T) {
	t.Helper()
	if !reflect.DeepEqual(cliV, apiV) {
		t.Errorf("%s differ:\nCLI %+v\nAPI %+v", what, cliV, apiV)
	}
}

// TestStreams runs logs, a pull, terminals and attach through the API on
// real docker and/or podman. It creates containers named "cui-it-api-*"
// from busybox:stable (pulled if missing) and removes them afterwards.
func TestStreams(t *testing.T) {
	for _, f := range []*cli.Flavor{cli.Docker, cli.Podman} {
		t.Run(f.Name, func(t *testing.T) {
			c, err := cli.New(f, config.Runtime{})
			if err != nil {
				t.Skip(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			if _, err := c.Info(ctx); err != nil {
				t.Skip("daemon not answering: ", err)
			}
			sock := c.APISocket(ctx)
			if sock == "" {
				t.Skip("no API socket")
			}
			a := Wrap(c, sock, Options{})
			const image = "docker.io/library/busybox:stable"

			var lines []string
			if err := a.PullImage(ctx, image, func(l string) { lines = append(lines, l) }); err != nil {
				t.Fatalf("pull: %v", err)
			}
			t.Logf("pull progress: %q", lines)
			if len(lines) == 0 || a.Source() != "api" {
				t.Errorf("pull: no progress lines or not through the API (source %q)", a.Source())
			}

			suffix := fmt.Sprint(time.Now().UnixNano())
			run := func(name string, args ...string) string {
				t.Helper()
				full := append([]string{"run", "-d", "--name", name}, args...)
				out, err := exec.Command(c.Path(), full...).CombinedOutput()
				if err != nil {
					t.Fatalf("%s run: %v: %s", f.Name, err, out)
				}
				t.Cleanup(func() { _ = exec.Command(c.Path(), "rm", "-f", name).Run() })
				return strings.TrimSpace(string(out))
			}
			plain := run("cui-it-api-plain-"+suffix, image, "sh", "-c", "echo out1; echo err1 >&2; echo out2; exec sleep 600")
			tty := run("cui-it-api-tty-"+suffix, "-it", image, "sh")
			time.Sleep(time.Second)

			// Logs: the same lines as the CLI (stdout and stderr may arrive
			// in another order).
			var viaAPI, viaCLI bytes.Buffer
			if err := a.ContainerLogs(ctx, plain, runtime.LogsOpts{}, &viaAPI); err != nil {
				t.Fatal(err)
			}
			if err := c.ContainerLogs(ctx, plain, runtime.LogsOpts{}, &viaCLI); err != nil {
				t.Fatal(err)
			}
			sorted := func(s string) []string { l := strings.Fields(s); sort.Strings(l); return l }
			if got, want := sorted(viaAPI.String()), sorted(viaCLI.String()); !reflect.DeepEqual(got, want) || len(got) != 3 {
				t.Errorf("logs %q, CLI %q", viaAPI.String(), viaCLI.String())
			}
			fctx, fcancel := context.WithTimeout(ctx, 2*time.Second)
			var follow bytes.Buffer
			err = a.ContainerLogs(fctx, plain, runtime.LogsOpts{Follow: true, Tail: 1}, &follow)
			fcancel()
			// The daemon keeps no strict order between stdout and stderr.
			if last := strings.TrimSpace(follow.String()); !errors.Is(err, context.DeadlineExceeded) || (last != "out2" && last != "err1") {
				t.Errorf("follow --tail 1: %q, %v", follow.String(), err)
			}

			// Terminal: run a command, resize, exit with a code.
			shell, err := a.CheckTerminal(ctx, plain)
			if err != nil || shell != "/bin/sh" {
				t.Fatalf("check terminal: %q, %v", shell, err)
			}
			term, err := a.OpenTerminal(ctx, plain, runtime.TerminalOpts{Shell: shell, Cols: 100, Rows: 30})
			if err != nil {
				t.Fatal(err)
			}
			readUntil(t, term, "hi-3", func() { _, _ = term.Write([]byte("echo hi-$((1+2)); stty size\r")) })
			if err := term.Resize(120, 40); err != nil {
				t.Errorf("resize: %v", err)
			}
			readUntil(t, term, "40 120", func() { _, _ = term.Write([]byte("stty size\r")) })
			_, _ = term.Write([]byte("exit 7\r"))
			_, _ = io.Copy(io.Discard, term)
			if code, err := term.Wait(); code != 7 || err != nil {
				t.Errorf("exit code %d, %v", code, err)
			}
			_ = term.Close()

			// Closing a terminal whose shell still runs ends the shell.
			before := processes(t, c.Path(), plain)
			term, err = a.OpenTerminal(ctx, plain, runtime.TerminalOpts{Shell: shell, Cols: 80, Rows: 24})
			if err != nil {
				t.Fatal(err)
			}
			readUntil(t, term, "ready", func() { _, _ = term.Write([]byte("echo ready\r")) })
			if during := processes(t, c.Path(), plain); during <= before {
				t.Errorf("no shell process while the terminal is open (%d, before %d)", during, before)
			}
			if err := term.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
			if after := processes(t, c.Path(), plain); after != before {
				t.Errorf("%d processes after close, %d before", after, before)
			}

			// Attach to the TTY container, type, detach: it keeps running.
			at, err := a.OpenAttach(ctx, tty, runtime.AttachOpts{Stdin: true, TTY: true, Cols: 80, Rows: 24})
			if err != nil {
				t.Fatal(err)
			}
			readUntil(t, at, "attached-ok", func() { _, _ = at.Write([]byte("echo attached-ok\r")) })
			if err := at.Close(); err != nil {
				t.Errorf("detach: %v", err)
			}
			if code, err := at.Wait(); code != 0 || err != nil {
				t.Errorf("after detach: %d, %v", code, err)
			}
			cs, err := a.ListContainers(ctx, runtime.ListContainersOpts{All: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, x := range cs {
				if strings.HasPrefix(tty, x.ID) || x.ID == tty {
					if !x.State.IsRunning() {
						t.Errorf("tty container %s after detach", x.State)
					}
				}
			}
		})
	}
}

// readUntil calls send, then reads r until want shows up (10 s at most).
func readUntil(t *testing.T, r io.Reader, want string, send func()) {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		var all []byte
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			all = append(all, buf[:n]...)
			if strings.Contains(string(all), want) || err != nil {
				got <- string(all)
				return
			}
		}
	}()
	send()
	select {
	case s := <-got:
		if !strings.Contains(s, want) {
			t.Fatalf("output %q without %q", s, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no %q in time", want)
	}
}

// processes counts the processes in the container (excluding ps itself
// and the CLI's exec), after cleanups settled.
func processes(t *testing.T, cliPath, id string) int {
	t.Helper()
	time.Sleep(1500 * time.Millisecond)
	out, err := exec.Command(cliPath, "exec", id, "ps", "-o", "pid,args").CombinedOutput()
	if err != nil {
		t.Fatalf("ps: %v: %s", err, out)
	}
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n")[1:] {
		if !strings.Contains(l, "ps -o") {
			n++
		}
	}
	return n
}
