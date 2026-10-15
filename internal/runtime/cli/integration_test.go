//go:build integration

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// TestLifecycle runs against the real wslc.exe:
//
//	go test -tags integration ./internal/runtime/cli/...
//
// It only touches resources it creates (prefixed "cui-it-").
func TestLifecycle(t *testing.T) {
	d, err := New(WSLC, config.Runtime{})
	if err != nil {
		t.Skip(err)
	}
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	name := "cui-it-" + suffix
	tag := "alpine:cui-it-" + suffix

	info, err := d.Info(ctx)
	if err != nil || info.Version == "" {
		t.Fatalf("info = %+v, %v", info, err)
	}

	var lines int
	if err := d.PullImage(ctx, "alpine:latest", func(string) { lines++ }); err != nil {
		t.Fatal(err)
	}
	if lines == 0 {
		t.Error("pull produced no progress lines")
	}
	if err := d.TagImage(ctx, "alpine:latest", tag); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.RemoveImage(ctx, tag, true) })
	if !hasImage(t, d, tag) {
		t.Fatalf("tag %s not listed", tag)
	}

	id, err := d.CreateContainer(ctx, runtime.CreateContainerOpts{
		Image: tag, Name: name, Cmd: []string{"sleep", "300"}, Env: []string{"CUI=1"}, Start: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.RemoveContainer(ctx, name, true) })
	if state(t, d, name) != runtime.StateRunning {
		t.Fatalf("container %s (%s) not running after create", name, id)
	}

	if _, err := d.InspectContainer(ctx, name); err != nil {
		t.Fatal(err)
	}
	stopTimeout := time.Second
	if err := d.RestartContainer(ctx, name, &stopTimeout); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveContainer(ctx, name, false); !errors.Is(err, runtime.ErrConflict) {
		t.Errorf("remove running container err = %v; want ErrConflict", err)
	}
	if err := d.StopContainer(ctx, name, &stopTimeout); err != nil {
		t.Fatal(err)
	}
	if st := state(t, d, name); st.IsRunning() {
		t.Fatalf("state after stop = %s", st)
	}
	if err := d.RemoveContainer(ctx, name, false); err != nil {
		t.Fatal(err)
	}
	if err := d.StopContainer(ctx, name, nil); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("stop removed container err = %v; want ErrNotFound", err)
	}
	if err := d.RemoveImage(ctx, tag, false); err != nil {
		t.Fatal(err)
	}
	if hasImage(t, d, tag) {
		t.Error("tag still listed after remove")
	}

	if _, err := d.ListVolumes(ctx); err != nil {
		t.Error(err)
	}
	if nets, err := d.ListNetworks(ctx); err != nil || len(nets) == 0 {
		t.Errorf("networks = %v, %v", nets, err)
	}
}

func hasImage(t *testing.T, d *Driver, ref string) bool {
	t.Helper()
	imgs, err := d.ListImages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range imgs {
		if i.Ref() == ref {
			return true
		}
	}
	return false
}

func state(t *testing.T, d *Driver, name string) runtime.State {
	t.Helper()
	cs, err := d.ListContainers(context.Background(), runtime.ListContainersOpts{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Name == name {
			return c.State
		}
	}
	return ""
}

// TestTerminal opens a shell on a pseudo terminal in a real container.
func TestTerminal(t *testing.T) {
	d, err := New(WSLC, config.Runtime{})
	if err != nil {
		t.Skip(err)
	}
	ctx := context.Background()
	name := "cui-it-term-" + fmt.Sprint(time.Now().UnixNano())
	if _, err := d.CreateContainer(ctx, runtime.CreateContainerOpts{
		Image: "alpine:latest", Name: name, Cmd: []string{"sleep", "300"}, Pull: runtime.PullMissing, Start: true,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.RemoveContainer(ctx, name, true) })

	shell, err := d.CheckTerminal(ctx, name)
	if err != nil || shell != "/bin/sh" {
		t.Fatalf("CheckTerminal = %q, %v; want /bin/sh", shell, err)
	}
	term, err := d.OpenTerminal(ctx, name, runtime.TerminalOpts{Shell: shell, Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	output := make(chan string, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				output <- string(buf[:n])
			}
			if err != nil {
				close(output)
				return
			}
		}
	}()
	var seen strings.Builder
	waitFor := func(desc string, ok func(string) bool) {
		t.Helper()
		timeout := time.After(15 * time.Second)
		for !ok(seen.String()) {
			select {
			case s, open := <-output:
				if !open {
					t.Fatalf("terminal closed before %s; output:\n%s", desc, seen.String())
				}
				seen.WriteString(s)
			case <-timeout:
				t.Fatalf("timed out waiting for %s; output:\n%s", desc, seen.String())
			}
		}
	}
	// The shell's line editor discards typeahead (also briefly after it
	// printed the prompt), so each command is sent shortly after its prompt.
	prompts := 0
	run := func(cmd, want string) {
		t.Helper()
		prompts++
		waitFor("prompt", func(s string) bool { return strings.Count(s, "/ # ") >= prompts })
		time.Sleep(500 * time.Millisecond)
		if _, err := term.Write([]byte(cmd + "\r")); err != nil {
			t.Fatal(err)
		}
		if want != "" {
			waitFor(want, func(s string) bool { return strings.Contains(s, want) })
		}
	}
	run("echo $((6*7))", "42")
	run("stty size", "30 100")
	if err := term.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	run("stty size", "40 120")
	run("exit 5", "")

	done := make(chan struct{})
	var code int
	go func() { code, err = term.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("shell did not exit")
	}
	if err != nil || code != 5 {
		t.Errorf("exit = %d, %v; want 5", code, err)
	}

	// Closing a live terminal (browser gone) ends wslc and the shell.
	term2, err := d.OpenTerminal(ctx, name, runtime.TerminalOpts{Shell: shell, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, term2) }()
	time.Sleep(time.Second)
	start := time.Now()
	_ = term2.Close()
	if _, err := term2.Wait(); err != nil {
		t.Error(err)
	}
	t.Logf("close took %s", time.Since(start).Round(time.Millisecond))
	time.Sleep(time.Second)
	out, err := d.run(ctx, "ps", "container", "exec", name, "ps", "-o", "pid,comm")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(out), " sh"); n != 0 {
		t.Errorf("%d shells still running after Close:\n%s", n, out)
	}

	if err := d.StopContainer(ctx, name, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CheckTerminal(ctx, name); err == nil {
		t.Error("CheckTerminal on a stopped container succeeded")
	} else {
		t.Logf("stopped container: %v", err)
	}
}
