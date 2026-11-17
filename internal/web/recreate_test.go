package web

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/registry"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

func (e *env) container(name string) *runtime.Container {
	e.t.Helper()
	cs, _ := e.rt.ListContainers(context.Background(), runtime.ListContainersOpts{All: true})
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}

func TestEditContainer(t *testing.T) {
	e := newEnv(t)
	e.login()

	w := e.do("GET", "/containers/web/edit", nil)
	body := w.Body.String()
	for _, want := range []string{"Edit Container web", `action="/containers/c0ffee000001/edit"`, `value="nginx:1.27"`, "127.0.0.1:8080:80", "data:/data/data", "Recreate container", `data-confirm="Recreate container web`} {
		if !strings.Contains(body, want) {
			t.Errorf("edit page misses %q", want)
		}
	}
	if !strings.Contains(e.do("GET", "/containers/web", nil).Body.String(), `href="/containers/c0ffee000001/edit"`) {
		t.Error("detail page has no Edit button")
	}

	form := url.Values{
		"image": {"nginx:1.27"}, "name": {"web"}, "ports": {"127.0.0.1:8080:80"}, "volumes": {"data:/data/data\ncache:/cache"},
		"network": {"backend"}, "network_aliases": {"www"}, "extra_networks": {"bridge"}, "pull": {"missing"}, "start": {"on"},
		"tty": {"on"}, "interactive": {"on"}, "restart": {"always"}, "labels": {"team=web"},
	}
	w = e.post("/containers/c0ffee000001/edit", form)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/containers/web" {
		t.Fatalf("edit = %d %s\n%s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	e.jobs.Wait()
	got := e.rt.LastCreate
	if len(got.Volumes) != 2 || got.RestartPolicy != "always" || !slices.Equal(got.Labels, []string{"team=web"}) ||
		!reflect.DeepEqual(got.Networks, []runtime.NetworkAttachment{{Name: "backend", Aliases: []string{"www"}}, {Name: "bridge"}}) {
		t.Errorf("create opts = %+v", got)
	}
	c := e.container("web")
	if c == nil || c.ID == "c0ffee000001" || c.State != runtime.StateRunning {
		t.Fatalf("recreated container = %+v", c)
	}
	page := e.do("GET", "/containers/web", nil).Body.String()
	if !strings.Contains(page, "Recreating container web") || !strings.Contains(page, "succeeded") {
		t.Error("detail page shows neither the flash nor the finished job")
	}
	// The poll after the job finished reloads the page.
	if w := e.do("GET", "/partials/containers/web/job?poll=1", nil, htmx); w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("job partial headers = %v", w.Header())
	}
	if !strings.Contains(e.logs.String(), "container recreated") {
		t.Error("recreate not logged")
	}

	// Invalid input re-renders the edit form.
	form.Set("ports", "notaport")
	w = e.post("/containers/"+c.ID+"/edit", form)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "invalid port") || !strings.Contains(w.Body.String(), "Edit Container web") {
		t.Errorf("invalid edit = %d", w.Code)
	}
}

func TestEditComposeContainerRefused(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.do("GET", "/containers/shop-web-1/edit", nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("edit = %d", w.Code)
	}
	page := e.do("GET", "/containers/shop-web-1", nil).Body.String()
	if !strings.Contains(page, "compose project shop") || strings.Contains(page, "/edit\"") {
		t.Error("compose container: missing notice or Edit button shown")
	}
	if w := e.post("/containers/shop-web-1/update", url.Values{"image": {"nginx:1.28"}}, htmx); w.Header().Get("HX-Redirect") != "/containers/shop-web-1" {
		t.Errorf("update redirect = %q", w.Header().Get("HX-Redirect"))
	}
	e.jobs.Wait()
	if slices.ContainsFunc(e.rt.Calls, func(c string) bool { return strings.HasPrefix(c, "create") }) {
		t.Error("compose container was recreated")
	}
}

type stubChecker struct {
	status registry.Status
	ref    string
	force  bool
}

func (s *stubChecker) Check(_ context.Context, ref string, _ []string, force bool) registry.Status {
	s.ref, s.force = ref, force
	st := s.status
	st.Ref = ref
	return st
}

func TestImageUpdate(t *testing.T) {
	stub := &stubChecker{status: registry.Status{DigestChanged: true, NewerTags: []registry.Tag{{Name: "1.28", Kind: "minor"}}, CheckedAt: time.Now()}}
	e := newEnvWith(t, func(o *Options) { o.Updates = stub })
	e.login()

	if !strings.Contains(e.do("GET", "/containers/web", nil).Body.String(), `hx-get="/partials/containers/c0ffee000001/update"`) {
		t.Error("detail page does not load the update check")
	}
	body := e.do("GET", "/partials/containers/c0ffee000001/update?refresh=1", nil, htmx).Body.String()
	if stub.ref != "nginx:1.27" || !stub.force {
		t.Errorf("checked %q force=%v", stub.ref, stub.force)
	}
	for _, want := range []string{"newer image for <code>nginx:1.27</code>", `name="image" value="nginx:1.27"`, `<option value="nginx:1.28">1.28 (minor)</option>`} {
		if !strings.Contains(body, want) {
			t.Errorf("update panel misses %q:\n%s", want, body)
		}
	}

	w := e.post("/containers/c0ffee000001/update", url.Values{"image": {"nginx:1.28"}}, htmx)
	if w.Header().Get("HX-Redirect") != "/containers/web" {
		t.Fatalf("update redirect = %q", w.Header().Get("HX-Redirect"))
	}
	e.jobs.Wait()
	c := e.container("web")
	if c == nil || c.Image != "nginx:1.28" || c.State != runtime.StateRunning {
		t.Fatalf("updated container = %+v", c)
	}
	if !slices.Contains(e.rt.Calls, "pull nginx:1.28") || e.rt.LastCreate.Ports[0].HostPort != 8080 {
		t.Errorf("calls %q, create %+v", e.rt.Calls, e.rt.LastCreate)
	}

	// Only tags of the container's own repository are accepted.
	e.post("/containers/"+c.ID+"/update", url.Values{"image": {"alpine:latest"}}, htmx)
	if !strings.Contains(e.do("GET", "/containers/web", nil).Body.String(), "is not an image of nginx") {
		t.Error("foreign image not refused")
	}

	stub.status = registry.Status{Err: "access denied"}
	if body := e.do("GET", "/partials/containers/web/update", nil, htmx).Body.String(); !strings.Contains(body, "Could not check <code>nginx:1.28</code>: access denied") {
		t.Errorf("error panel:\n%s", body)
	}
}

func TestImageUpdateDisabled(t *testing.T) {
	e := newEnv(t)
	e.login()
	if strings.Contains(e.do("GET", "/containers/web", nil).Body.String(), "/update\"") {
		t.Error("update check shown without a checker")
	}
	if w := e.do("GET", "/partials/containers/web/update", nil, htmx); w.Code != http.StatusNoContent {
		t.Errorf("partial = %d", w.Code)
	}
}

func TestFormFromOptsRoundTrip(t *testing.T) {
	opts := runtime.CreateContainerOpts{
		Image: "nginx:1.27", Name: "web", Cmd: []string{"sh", "-c", `echo "hi there"`}, Env: []string{"A=1", "B=2"},
		Ports:    []runtime.PortMapping{{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80}, {ContainerPort: 53, Protocol: "udp"}},
		Volumes:  []runtime.VolumeMount{{Source: "data", Target: "/data", ReadOnly: true}},
		Networks: []runtime.NetworkAttachment{{Name: "front", Aliases: []string{"www"}, IP: "172.30.0.5"}, {Name: "back", Aliases: []string{"api", "db"}}},
		Pull:     runtime.PullMissing, Start: true, TTY: true, AutoRemove: true, CPUs: "1.5", Memory: "512m",
		DNS: []string{"1.1.1.1"}, Ulimits: []string{"nofile=1024:2048"}, RestartPolicy: "on-failure:3",
		Labels: []string{"a=b c"}, Entrypoint: []string{"/entry.sh", "--flag"}, WorkDir: "/srv",
		ExtraHosts: []string{"db:10.0.0.5"}, CapAdd: []string{"NET_ADMIN"}, CapDrop: []string{"MKNOD"}, Privileged: true,
	}
	f := formFromOpts(opts)
	got, err := f.toOpts()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, opts) {
		t.Errorf("round trip =\n %+v\nwant\n %+v", got, opts)
	}
	if !slices.Contains(f.RestartPolicies(), "on-failure:3") {
		t.Error("custom restart policy missing from the select")
	}
}
