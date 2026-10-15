package cli

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

func TestParseLabels(t *testing.T) {
	got := parseLabels(`com.microsoft.wsl.container.metadata={"V1":{"Flags":0,"Ports":[{"HostPort":5984,"Protocol":6}],"Volumes":[]}},com.docker.compose.project=shop,org.opencontainers.image.description=a, b and c,empty=`)
	want := map[string]string{
		"com.microsoft.wsl.container.metadata": `{"V1":{"Flags":0,"Ports":[{"HostPort":5984,"Protocol":6}],"Volumes":[]}}`,
		"com.docker.compose.project":           "shop",
		"org.opencontainers.image.description": "a, b and c",
		"empty":                                "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseLabels = %#v", got)
	}
	if parseLabels("") != nil {
		t.Error("empty labels should be nil")
	}
}

func TestListParsesLabels(t *testing.T) {
	d := NewWithRunner(&fakeRunner{stdout: fixture(t, "docker_containers.ndjson")}, Docker, config.Runtime{})
	cs, err := d.ListContainers(context.Background(), runtime.ListContainersOpts{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if cs[0].Labels["maintainer"] != "NGINX Docker Maintainers" {
		t.Errorf("labels = %#v", cs[0].Labels)
	}

	podman := `[{"Id":"abc","Names":["shop-web-1"],"Labels":{"com.docker.compose.project":"shop","com.docker.compose.service":"web"},"State":"running"}]`
	d = NewWithRunner(&fakeRunner{stdout: podman}, Podman, config.Runtime{})
	cs, err = d.ListContainers(context.Background(), runtime.ListContainersOpts{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if cs[0].Labels[runtime.LabelProject] != "shop" || cs[0].Labels[runtime.LabelService] != "web" {
		t.Errorf("podman labels = %#v", cs[0].Labels)
	}

	vols := `{"Driver":"local","Labels":"com.docker.compose.project=shop,com.docker.compose.volume=data","Name":"shop_data","Scope":"local"}`
	d = NewWithRunner(&fakeRunner{stdout: vols}, Docker, config.Runtime{})
	vs, err := d.ListVolumes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if vs[0].Labels[runtime.LabelVolume] != "data" {
		t.Errorf("volume labels = %#v", vs[0].Labels)
	}
}

func TestParseComposeVersion(t *testing.T) {
	for _, tc := range []struct {
		out     string
		kind    composeKind
		version string
		ok      bool
	}{
		{"Docker Compose version v2.29.1\n", composeV2, "2.29.1", true},
		{"Docker Compose version 2.24.6-desktop.1", composeV2, "2.24.6-desktop.1", true},
		{">>>> Executing external compose provider \"/usr/bin/podman-compose\". <<<<\npodman-compose version: 1.0.6\n['podman', '--version', '']\nusing podman version: 4.9.3\npodman-compose version 1.0.6\npodman --version \npodman version 4.9.3", podmanCompose, "1.0.6", true},
		{"podman-compose version 1.2.0\npodman version 5.0.0", podmanCompose, "1.2.0", true},
		{"docker-compose version 1.29.2, build 5becea4c", 0, "", false},
		{"no version here", 0, "", false},
	} {
		kind, version, ok := parseComposeVersion(tc.out)
		if kind != tc.kind || version != tc.version || ok != tc.ok {
			t.Errorf("parseComposeVersion(%q) = %v, %q, %t", tc.out, kind, version, ok)
		}
	}
}

func TestFindCompose(t *testing.T) {
	missingPlugin := &fakeRunner{stderr: "docker: 'compose' is not a docker command.", code: 1}
	standalone := &fakeRunner{stdout: "Docker Compose version v2.29.1"}
	lookup := func(bin string) (Runner, bool) {
		if bin == "docker-compose" {
			return standalone, true
		}
		return nil, false
	}
	c := findCompose(Docker, config.Runtime{}, missingPlugin, lookup)
	if c == nil || c.name != "docker-compose" || c.prefix != nil || c.version != "2.29.1" {
		t.Fatalf("findCompose = %+v", c)
	}
	if got := missingPlugin.last(); got != "compose version" {
		t.Errorf("plugin probe = %q", got)
	}

	plugin := &fakeRunner{stdout: "Docker Compose version v2.30.0"}
	c = findCompose(Docker, config.Runtime{}, plugin, lookup)
	if c == nil || c.name != "docker compose" || !reflect.DeepEqual(c.prefix, []string{"compose"}) {
		t.Fatalf("findCompose plugin = %+v", c)
	}

	if c := findCompose(Docker, config.Runtime{}, missingPlugin, func(string) (Runner, bool) { return nil, false }); c != nil {
		t.Errorf("no tool installed: %+v", c)
	}
	if c := findCompose(WSLC, config.Runtime{}, plugin, lookup); c != nil {
		t.Errorf("wslc has no compose: %+v", c)
	}

	// compose_path replaces the candidates.
	var asked string
	c = findCompose(Podman, config.Runtime{ComposePath: "/opt/podman-compose"}, plugin, func(bin string) (Runner, bool) {
		asked = bin
		return &fakeRunner{stdout: "podman-compose version 1.2.0"}, true
	})
	if asked != "/opt/podman-compose" || c == nil || c.kind != podmanCompose || c.name != "podman-compose" {
		t.Errorf("compose_path: asked %q, got %+v", asked, c)
	}
}

func composeDriver(r *fakeRunner, kind composeKind) *Driver {
	d := NewWithRunner(r, Docker, config.Runtime{})
	d.compose = &composeTool{runner: r, prefix: []string{"compose"}, name: "docker compose", kind: kind, version: "2.29.1"}
	if kind == podmanCompose {
		d.compose.prefix, d.compose.name = nil, "podman-compose"
	}
	return d
}

func TestComposeCommands(t *testing.T) {
	file := filepath.Join(t.TempDir(), "compose.yaml")
	target := runtime.ComposeTarget{Project: "shop", File: file}
	ctx := context.Background()

	r := &fakeRunner{stdout: " Container shop-web-1  Started\r\n Container shop-db-1  Started\n"}
	d := composeDriver(r, composeV2)
	var lines []string
	if err := d.ComposeUp(ctx, target, func(l string) { lines = append(lines, l) }); err != nil {
		t.Fatal(err)
	}
	if got, want := r.last(), "compose --ansi never -p shop -f "+file+" up --detach --remove-orphans"; got != want {
		t.Errorf("up = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(lines, []string{"Container shop-web-1  Started", "Container shop-db-1  Started"}) {
		t.Errorf("progress = %q", lines)
	}

	if err := d.ComposeAction(ctx, runtime.ComposeTarget{Project: "shop"}, "restart", nil); err != nil {
		t.Fatal(err)
	}
	if got := r.last(); got != "compose --ansi never -p shop restart" {
		t.Errorf("restart = %q", got)
	}
	if err := d.ComposeDown(ctx, target, true, nil); err != nil {
		t.Fatal(err)
	}
	if got := r.last(); got != "compose --ansi never -p shop -f "+file+" down --volumes" {
		t.Errorf("down = %q", got)
	}
	if err := d.ComposeValidate(ctx, target); err != nil {
		t.Fatal(err)
	}
	if got := r.last(); got != "compose --ansi never -p shop -f "+file+" config --quiet" {
		t.Errorf("config = %q", got)
	}

	p := &fakeRunner{}
	d = composeDriver(p, podmanCompose)
	if err := d.ComposeUp(ctx, target, nil); err != nil {
		t.Fatal(err)
	}
	if got := p.last(); got != "-p shop -f "+file+" up --detach --remove-orphans" {
		t.Errorf("podman-compose up = %q", got)
	}
	if err := d.ComposeValidate(ctx, target); err != nil {
		t.Fatal(err)
	}
	if got := p.last(); got != "-p shop -f "+file+" config" {
		t.Errorf("podman-compose config = %q", got)
	}
}

func TestComposeErrors(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "compose.yaml")

	d := NewWithRunner(&fakeRunner{}, Docker, config.Runtime{})
	if d.Capabilities().Compose {
		t.Error("Compose capability without a tool")
	}
	if err := d.ComposeUp(ctx, runtime.ComposeTarget{Project: "shop", File: file}, nil); !errors.Is(err, runtime.ErrUnsupported) {
		t.Errorf("no tool: %v", err)
	}

	r := &fakeRunner{stdout: "pulling\nError response from daemon: pull access denied for nope\n", code: 1}
	d = composeDriver(r, composeV2)
	if !d.Capabilities().Compose || d.Capabilities().ComposeVersion != "docker compose 2.29.1" {
		t.Errorf("caps = %+v", d.Capabilities())
	}
	err := d.ComposeUp(ctx, runtime.ComposeTarget{Project: "shop", File: file}, nil)
	var ce *runtime.CommandError
	if !errors.As(err, &ce) || ce.ExitCode != 1 || ce.Output != "pulling Error response from daemon: pull access denied for nope" {
		t.Errorf("up error = %#v", err)
	}

	for _, bad := range []runtime.ComposeTarget{
		{Project: "Shop", File: file},
		{Project: "-p", File: file},
		{Project: "shop", File: "relative.yaml"},
		{Project: "shop"}, // up needs a file
	} {
		if err := d.ComposeUp(ctx, bad, nil); !errors.Is(err, runtime.ErrInvalid) {
			t.Errorf("ComposeUp(%+v) = %v, want ErrInvalid", bad, err)
		}
	}
	if err := d.ComposeAction(ctx, runtime.ComposeTarget{Project: "shop"}, "rm", nil); !errors.Is(err, runtime.ErrInvalid) {
		t.Errorf("unknown action: %v", err)
	}

	v := &fakeRunner{stderr: "services.web.ports must be a list", code: 15}
	d = composeDriver(v, composeV2)
	if err := d.ComposeValidate(ctx, runtime.ComposeTarget{Project: "shop", File: file}); !errors.Is(err, runtime.ErrInvalid) || !errors.As(err, &ce) || ce.Output != "services.web.ports must be a list" {
		t.Errorf("validate = %v", err)
	}
}

func TestInspectImage(t *testing.T) {
	r := &fakeRunner{stdout: `[{"Id":"sha256:abc","Config":{"Env":["PATH=/bin"]}}]`}
	d := NewWithRunner(r, Docker, config.Runtime{})
	raw, err := d.InspectImage(context.Background(), "nginx:1.27")
	if err != nil || !strings.Contains(string(raw), "sha256:abc") {
		t.Fatalf("InspectImage = %s, %v", raw, err)
	}
	if got := r.last(); got != "image inspect nginx:1.27" {
		t.Errorf("args = %q", got)
	}
	d = NewWithRunner(&fakeRunner{stdout: "[]", stderr: "Error: No such image: nope", code: 1}, Docker, config.Runtime{})
	if _, err := d.InspectImage(context.Background(), "nope"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("missing image: %v", err)
	}
	if _, err := d.InspectImage(context.Background(), "-rf"); !errors.Is(err, runtime.ErrInvalid) {
		t.Errorf("flag-like ref: %v", err)
	}
}
