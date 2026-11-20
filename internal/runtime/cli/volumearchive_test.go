package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

const inspectData = `[{"Name":"data","Driver":"local","Mountpoint":"/var/lib/docker/volumes/data/_data"}]`

func TestExportVolume(t *testing.T) {
	r := &fakeRunner{queue: []string{inspectData}, exchangeOut: "TARDATA"}
	d := NewWithRunner(r, Docker, config.Runtime{})
	var buf bytes.Buffer
	if err := d.ExportVolume(context.Background(), "data", &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "TARDATA" {
		t.Errorf("stdout = %q", buf.String())
	}
	want := "container run --rm --interactive --quiet --network none --log-driver none --user 0:0 --label containerui.helper=volume-archive --volume data:/data:ro --entrypoint sh docker.io/library/busybox:stable -c cd /data && exec tar -cf - ."
	if len(r.calls) != 2 || strings.Join(r.calls[0], " ") != "volume inspect data" || strings.Join(r.calls[1], " ") != want {
		t.Errorf("calls = %q", r.calls)
	}
}

func TestImportVolume(t *testing.T) {
	for _, replace := range []bool{false, true} {
		r := &fakeRunner{queue: []string{inspectData}}
		d := NewWithRunner(r, Podman, config.Runtime{VolumeHelperImage: "quay.io/me/tools:1"})
		if err := d.ImportVolume(context.Background(), "data", strings.NewReader("TAR"), replace); err != nil {
			t.Fatal(err)
		}
		args := strings.Join(r.calls[1], " ")
		if !strings.Contains(args, "--volume data:/data --entrypoint sh quay.io/me/tools:1 -c") || string(r.exchangeIn) != "TAR" {
			t.Errorf("replace=%v: %q, stdin %q", replace, args, r.exchangeIn)
		}
		if got := strings.Contains(args, "find . -mindepth 1 -delete"); got != replace {
			t.Errorf("replace=%v: %q", replace, args)
		}
	}
}

func TestVolumeArchiveErrors(t *testing.T) {
	// A missing volume is not created by the helper's --volume.
	r := &fakeRunner{queue: []string{"[]"}, code: 1, stderr: "Error: no such volume: nope"}
	d := NewWithRunner(r, Docker, config.Runtime{})
	if err := d.ExportVolume(context.Background(), "nope", &bytes.Buffer{}); !errors.Is(err, runtime.ErrNotFound) || len(r.calls) != 1 {
		t.Errorf("err = %v, calls %q", err, r.calls)
	}
	// The helper's failure carries its stderr.
	r = &fakeRunner{queue: []string{inspectData}, code: 2, stderr: "tar: write error: No space left on device"}
	d = NewWithRunner(r, Docker, config.Runtime{})
	err := d.ImportVolume(context.Background(), "data", strings.NewReader(""), false)
	if err == nil || !strings.Contains(err.Error(), "No space left") {
		t.Errorf("err = %v", err)
	}
	// wslc: not verified yet.
	d = NewWithRunner(&fakeRunner{}, WSLC, config.Runtime{})
	if d.Capabilities().VolumeBackup {
		t.Error("wslc reports volume backup")
	}
	if err := d.ExportVolume(context.Background(), "data", &bytes.Buffer{}); !errors.Is(err, runtime.ErrUnsupported) {
		t.Errorf("wslc export: %v", err)
	}
	if !NewWithRunner(&fakeRunner{}, Docker, config.Runtime{}).Capabilities().VolumeBackup {
		t.Error("docker reports no volume backup")
	}
}
