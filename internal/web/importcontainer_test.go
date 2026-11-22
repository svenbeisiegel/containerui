package web

import (
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

func TestImportInspect(t *testing.T) {
	e := newEnv(t)
	e.login()

	page := e.do("GET", "/containers/new/inspect", nil).Body.String()
	for _, want := range []string{`action="/containers/new/inspect"`, `name="text"`, `data-file-load`, "From docker inspect", `href="/containers/new" aria-label="Back"`} {
		if !strings.Contains(page, want) {
			t.Errorf("inspect page misses %q", want)
		}
	}

	raw, err := os.ReadFile("../runtime/testdata/inspect_compose_web.json")
	if err != nil {
		t.Fatal(err)
	}
	w := e.post("/containers/new/inspect", url.Values{"text": {string(raw)}})
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("import = %d\n%s", w.Code, body)
	}
	for _, want := range []string{
		`action="/containers"`, `value="nginx:1.27"`, `name="name" value="web"`, ">8082:80</textarea>",
		"shop_static:/usr/share/nginx/html:ro", "APP_ENV=production", "traefik.enable=true",
		`value="shop_default" selected`, `name="network_aliases" value="web"`, `<option value="unless-stopped" selected>`,
		`name="start" checked`, "Filled from container <strong>shop-web-1</strong>", "compose project <strong>shop</strong>",
		"Network shop_default belongs to compose project shop", "security option no-new-privileges:true",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("filled form misses %q", want)
		}
	}
	// The image is local: its environment is not repeated; compose's labels are dropped.
	for _, unwanted := range []string{"PATH=/usr/local", "com.docker.compose", "not available locally"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("filled form contains %q", unwanted)
		}
	}
	if slices.ContainsFunc(e.rt.Calls, func(c string) bool { return strings.HasPrefix(c, "create") }) {
		t.Error("import created a container")
	}

	// Submitting the filled form creates the container (renamed: "web" exists).
	form := url.Values{
		"image": {"nginx:1.27"}, "name": {"web2"}, "ports": {"8082:80"}, "volumes": {"shop_static:/usr/share/nginx/html:ro"},
		"env": {"APP_ENV=production\nTZ=Europe/Berlin"}, "network": {"shop_default"}, "network_aliases": {"web"},
		"labels": {"traefik.enable=true"}, "restart": {"unless-stopped"}, "pull": {"missing"}, "start": {"on"},
	}
	if w := e.post("/containers", form); w.Code != http.StatusSeeOther {
		t.Fatalf("create = %d\n%s", w.Code, w.Body.String())
	}
	got := e.rt.LastCreate
	if got.Name != "web2" || !slices.Equal(got.Env, []string{"APP_ENV=production", "TZ=Europe/Berlin"}) ||
		!reflect.DeepEqual(got.Networks, []runtime.NetworkAttachment{{Name: "shop_default", Aliases: []string{"web"}}}) {
		t.Errorf("create opts = %+v", got)
	}
}

func TestImportInspectRejected(t *testing.T) {
	e := newEnv(t)
	e.login()
	for name, tc := range map[string]struct{ in, want string }{
		"image":   {`[{"Id":"sha256:1","RepoTags":["nginx:1.27"],"RootFS":{}}]`, "inspect output of an image"},
		"garbage": {"docker inspect web", "not valid JSON"},
		"two":     {`[{"Id":"a"},{"Id":"b"}]`, "contains 2 objects"},
	} {
		w := e.post("/containers/new/inspect", url.Values{"text": {tc.in}})
		body := w.Body.String()
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d, missing %q", name, w.Code, tc.want)
		}
		if !strings.Contains(body, `action="/containers/new/inspect"`) {
			t.Errorf("%s: not back on the paste page", name)
		}
		// The paste is kept (HTML-escaped) for correcting it.
		if !strings.Contains(body, strings.ReplaceAll(tc.in, `"`, "&#34;")) {
			t.Errorf("%s: paste not kept", name)
		}
	}
	if w := e.post("/containers/new/inspect", url.Values{"text": {strings.Repeat(" ", maxImportPaste+1)}}); !strings.Contains(w.Body.String(), "larger than 2 MiB") {
		t.Errorf("oversized paste accepted: %d", w.Code)
	}
}

func TestEditHasNoImport(t *testing.T) {
	e := newEnv(t)
	e.login()
	if body := e.do("GET", "/containers/web/edit", nil).Body.String(); strings.Contains(body, "/containers/new/inspect") {
		t.Error("edit page offers the inspect import")
	}
}

func TestCreateChooser(t *testing.T) {
	e := newEnv(t)
	e.login()
	body := e.do("GET", "/containers/new", nil).Body.String()
	for _, want := range []string{`href="/containers/new/scratch"`, `href="/containers/new/inspect"`, `href="/containers/new/cli"`, `href="/containers/new/compose"`, `class="source-card"`} {
		if !strings.Contains(body, want) {
			t.Errorf("chooser misses %q", want)
		}
	}
	if strings.Contains(body, `name="image"`) {
		t.Error("chooser shows the create form")
	}
	// Run… on the Images page goes straight to the form.
	if body := e.do("GET", "/containers/new?image=redis:7", nil).Body.String(); !strings.Contains(body, `name="image" list="image-list" required value="redis:7"`) {
		t.Error("?image= does not open the filled form")
	}
	if w := e.do("GET", "/containers/new/bogus", nil); w.Code == http.StatusOK {
		t.Errorf("unknown source = %d", w.Code)
	}
}

func TestImportCLI(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.post("/containers/new/cli", url.Values{"text": {"docker run -d --name cache \\\n  -p 6379:6379 --restart always --device /dev/x \\\n  redis:7 redis-server --save ''"}})
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("import = %d\n%s", w.Code, body)
	}
	for _, want := range []string{
		`action="/containers"`, `value="redis:7"`, `name="name" value="cache"`, ">6379:6379</textarea>",
		`<option value="always" selected>`, `name="cmd" value="redis-server --save &#34;&#34;"`, `name="start" checked`,
		"Filled from the docker command", "--device /dev/x",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("filled form misses %q", want)
		}
	}
	// docker run without -it: no TTY, STDIN closed.
	if checkbox(body, "tty") || checkbox(body, "interactive") {
		t.Error("TTY or interactive set without -t / -i")
	}

	w = e.post("/containers/new/cli", url.Values{"text": {"docker compose up -d"}})
	if body := w.Body.String(); w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "This is a docker compose command") || !strings.Contains(body, "docker compose up -d</textarea>") {
		t.Errorf("compose command: %d\n%s", w.Code, body)
	}
}

func TestImportCompose(t *testing.T) {
	e := newEnv(t)
	e.login()
	file := "name: shop\nservices:\n  web:\n    image: nginx:1.27\n    networks: [default]\n  db:\n    image: postgres:16\n    environment:\n      POSTGRES_PASSWORD: secret\n"

	// Two services: the user chooses.
	w := e.post("/containers/new/compose", url.Values{"text": {file}})
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("import = %d\n%s", w.Code, body)
	}
	for _, want := range []string{`name="service" value="web" required checked`, `name="service" value="db" required>`, "postgres:16", "defines 2 services"} {
		if !strings.Contains(body, want) {
			t.Errorf("service choice misses %q", want)
		}
	}
	if strings.Contains(body, `action="/containers"`) {
		t.Error("form shown before choosing a service")
	}

	w = e.post("/containers/new/compose", url.Values{"text": {file}, "service": {"db"}})
	body = w.Body.String()
	for _, want := range []string{`value="postgres:16"`, `name="name" value="db"`, "POSTGRES_PASSWORD=secret", "service <strong>db</strong> of the compose file", "project <strong>shop</strong>"} {
		if !strings.Contains(body, want) {
			t.Errorf("filled form misses %q", want)
		}
	}

	// A single service block: no question; a network that does not exist is kept selected.
	w = e.post("/containers/new/compose", url.Values{"text": {"    web:\n      image: nginx:1.27\n      networks: [missingnet]\n"}})
	body = w.Body.String()
	for _, want := range []string{`value="nginx:1.27"`, `name="name" value="web"`, `<option value="missingnet" selected>missingnet (does not exist)</option>`} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment form misses %q", want)
		}
	}
	if slices.ContainsFunc(e.rt.Calls, func(c string) bool { return strings.HasPrefix(c, "create") }) {
		t.Error("import created a container")
	}

	w = e.post("/containers/new/compose", url.Values{"text": {"services:\n  app:\n    build: .\n"}})
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "build the image first") {
		t.Errorf("build-only service: %d", w.Code)
	}
}
