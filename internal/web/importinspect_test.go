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

	page := e.do("GET", "/containers/new", nil).Body.String()
	for _, want := range []string{`action="/containers/new/import"`, `name="inspect"`, `data-file-load`, `<details class="section advanced import-inspect">`} {
		if !strings.Contains(page, want) {
			t.Errorf("create page misses %q", want)
		}
	}

	raw, err := os.ReadFile("../runtime/testdata/inspect_compose_web.json")
	if err != nil {
		t.Fatal(err)
	}
	w := e.post("/containers/new/import", url.Values{"inspect": {string(raw)}})
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
		w := e.post("/containers/new/import", url.Values{"inspect": {tc.in}})
		body := w.Body.String()
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d, missing %q", name, w.Code, tc.want)
		}
		if !strings.Contains(body, `<details class="section advanced import-inspect" open>`) {
			t.Errorf("%s: import section not open", name)
		}
		// The paste is kept (HTML-escaped) for correcting it.
		if !strings.Contains(body, strings.ReplaceAll(tc.in, `"`, "&#34;")) {
			t.Errorf("%s: paste not kept", name)
		}
	}
	if w := e.post("/containers/new/import", url.Values{"inspect": {strings.Repeat(" ", maxInspectPaste+1)}}); !strings.Contains(w.Body.String(), "larger than 2 MiB") {
		t.Errorf("oversized paste accepted: %d", w.Code)
	}
}

func TestEditHasNoImport(t *testing.T) {
	e := newEnv(t)
	e.login()
	if body := e.do("GET", "/containers/web/edit", nil).Body.String(); strings.Contains(body, "/containers/new/import") {
		t.Error("edit page offers the inspect import")
	}
}
