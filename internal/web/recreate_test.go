package web

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/registry"
	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/updates"
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
	if !strings.Contains(e.do("GET", "/partials/containers", nil, htmx).Body.String(), `href="/containers/c0ffee000001/edit"`) {
		t.Error("container menu has no Edit item")
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
	if len(got.Volumes) != 2 || got.RestartPolicy != "always" || !slices.Equal(got.Labels, []string{"team=web", "containerui.pull=missing"}) ||
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
	if !strings.Contains(page, "compose project shop") {
		t.Error("compose container: missing notice")
	}
	list := e.do("GET", "/partials/containers", nil, htmx).Body.String()
	for _, item := range []string{"/edit\"", "/recreate\"", "/update\""} {
		if id := e.container("shop-web-1").ID; strings.Contains(list, "/containers/"+id+item) {
			t.Errorf("compose container menu offers %s", item)
		}
	}
	for _, act := range []string{"update", "recreate"} {
		if w := e.post("/containers/shop-web-1/"+act, url.Values{"image": {"nginx:1.28"}}, htmx); w.Header().Get("HX-Redirect") != "/containers" {
			t.Errorf("%s redirect = %q", act, w.Header().Get("HX-Redirect"))
		}
	}
	e.jobs.Wait()
	if slices.ContainsFunc(e.rt.Calls, func(c string) bool { return strings.HasPrefix(c, "create") }) {
		t.Error("compose container was recreated")
	}
}

// stubChecker answers update checks from a table (by reference); other
// references are up to date.
type stubChecker struct {
	mu     sync.Mutex
	status map[string]registry.Status
	forced int
}

func (s *stubChecker) set(ref string, st registry.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[ref] = st
}

func (s *stubChecker) forcedChecks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forced
}

func (s *stubChecker) Check(_ context.Context, ref string, _ []string, force bool) registry.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if force {
		s.forced++
	}
	st := s.status[ref]
	st.Ref, st.CheckedAt = ref, time.Now()
	return st
}

func newUpdateEnv(t *testing.T) (*env, *stubChecker, *updates.Scanner) {
	stub := &stubChecker{status: map[string]registry.Status{
		"nginx:1.27":    {NewerTags: []registry.Tag{{Name: "1.28", Kind: "minor"}}},
		"alpine:latest": {DigestChanged: true},
	}}
	var sc *updates.Scanner
	e := newEnvWith(t, func(o *Options) {
		sc = updates.New(o.Runtime, stub, time.Hour, o.Logger)
		o.Updates = sc
	})
	e.login()
	sc.Scan(context.Background(), false)
	return e, stub, sc
}

func TestImageUpdates(t *testing.T) {
	e, stub, sc := newUpdateEnv(t)

	// The container list marks outdated containers.
	list := e.do("GET", "/partials/containers", nil, htmx).Body.String()
	if n := strings.Count(list, `class="badge warn update-tag wide-only"`); n < 2 {
		t.Errorf("%d containers have an update tag; want web and job", n)
	}
	for _, want := range []string{`action="/containers/c0ffee000001/update"`, ">Update…<", `<option value="nginx:1.28">1.28 (minor)</option>`} {
		if !strings.Contains(list, want) {
			t.Errorf("container menu misses %q", want)
		}
	}
	// The images page offers to pull the newer images.
	imgs := e.do("GET", "/partials/images", nil, htmx).Body.String()
	for _, want := range []string{`name="ref" value="nginx:1.28"`, ">Pull 1.28<", `name="ref" value="alpine:latest"`, ">Update<", "Updates checked", "2 images have an update"} {
		if !strings.Contains(imgs, want) {
			t.Errorf("images table misses %q", want)
		}
	}

	// The check is a menu in the page head, left of Pull image and
	// Refresh: the state, the pull switch (off) and Check. Image partials
	// update only the state out of band, so an open menu stays open.
	page := e.do("GET", "/images", nil).Body.String()
	checkHeadActions(t, "/images", page, []string{"Check for updates", "Check", "Pull image", "Refresh"})
	for _, want := range []string{`<details class="menu">`, "Updates checked", `<input type="checkbox" name="pull" value="true">`, "Pull new images"} {
		if !strings.Contains(page, want) {
			t.Errorf("images page head misses %q", want)
		}
	}
	if !strings.Contains(imgs, `id="images-scan" class="scan-state" hx-swap-oob="true"`) {
		t.Error("images partial does not update the check state out of band")
	}
	if strings.Contains(imgs, `<details class="menu">`) {
		t.Error("images partial replaces the check menu")
	}

	// Pull the newer version: nginx:1.27 is no longer the latest local
	// image, but the container still runs it.
	e.post("/images/pull", url.Values{"ref": {"nginx:1.28"}}, htmx)
	e.jobs.Wait()
	imgs = e.do("GET", "/partials/images", nil, htmx).Body.String()
	if strings.Contains(imgs, ">Pull 1.28<") {
		t.Error("pull button still shown after pulling 1.28")
	}
	page = e.do("GET", "/containers/web", nil).Body.String()
	for _, want := range []string{"update-tag", "Image update", "nginx:1.28"} {
		if !strings.Contains(page, want) {
			t.Errorf("detail page misses %q", want)
		}
	}
	if !strings.Contains(detailHead(page), "/update\"") {
		t.Error("detail page head does not offer the update action")
	}

	w := e.post("/containers/c0ffee000001/update", url.Values{"image": {"nginx:1.28"}}, htmx)
	if w.Header().Get("HX-Redirect") != "/containers/web" {
		t.Fatalf("update redirect = %q", w.Header().Get("HX-Redirect"))
	}
	e.jobs.Wait()
	c := e.container("web")
	if c == nil || c.Image != "nginx:1.28" || c.State != runtime.StateRunning || e.rt.LastCreate.Ports[0].HostPort != 8080 {
		t.Fatalf("updated container = %+v", c)
	}
	pulls := 0
	for _, call := range e.rt.Calls {
		if call == "pull nginx:1.28" {
			pulls++
		}
	}
	if pulls != 1 {
		t.Errorf("nginx:1.28 pulled %d times; the local image should be used", pulls)
	}
	if page := e.do("GET", "/containers/web", nil).Body.String(); strings.Contains(page, "update-tag") {
		t.Error("updated container still shows an update")
	}

	// Only images the check found are accepted.
	e.post("/containers/"+c.ID+"/update", url.Values{"image": {"alpine:latest"}}, htmx)
	if !strings.Contains(e.do("GET", "/containers/web", nil).Body.String(), "no newer image alpine:latest is known") {
		t.Error("unknown image not refused")
	}

	// The tag was rebuilt: once the new image is pulled, the container
	// still runs the old one and is updated without pulling again.
	stub.set("alpine:latest", registry.Status{})
	e.post("/images/pull", url.Values{"ref": {"alpine:latest"}}, htmx)
	e.jobs.Wait()
	if list := e.do("GET", "/partials/containers", nil, htmx).Body.String(); !strings.Contains(list, `<option value="alpine:latest">latest (newer image already pulled)</option>`) {
		t.Errorf("job container: no update to the pulled image:\n%s", list)
	}

	// Check now asks every registry again.
	before := stub.forcedChecks()
	if body := e.post("/images/check-updates", nil, htmx).Body.String(); !strings.Contains(body, "Checking for updates") && !strings.Contains(body, "Updates checked") {
		t.Errorf("check-updates response:\n%s", body)
	}
	sc.Wait()
	if stub.forcedChecks() <= before {
		t.Error("check now did not force registry checks")
	}
}

// Check with "Pull new images" runs the check as a job and then pulls the
// rebuilt tag and the newest version; without it nothing is pulled.
func TestImageCheckUpdatesPull(t *testing.T) {
	e, _, sc := newUpdateEnv(t)
	pulls := func() []string {
		var out []string
		for _, c := range e.rt.Calls {
			if strings.HasPrefix(c, "pull ") {
				out = append(out, c)
			}
		}
		return out
	}

	e.post("/images/check-updates", nil, htmx)
	sc.Wait()
	e.jobs.Wait()
	if p := pulls(); len(p) != 0 {
		t.Fatalf("check without pull pulled %v", p)
	}

	w := e.post("/images/check-updates", url.Values{"pull": {"true"}}, htmx)
	if w.Header().Get("HX-Trigger") != "activity-changed" {
		t.Errorf("HX-Trigger = %q, want the activity drawer opened", w.Header().Get("HX-Trigger"))
	}
	if body := w.Body.String(); !strings.Contains(body, "Checking for updates") {
		t.Errorf("response does not show the running check:\n%s", body)
	}
	e.jobs.Wait()
	got := pulls()
	slices.Sort(got)
	if want := []string{"pull alpine:latest", "pull nginx:1.28"}; !slices.Equal(got, want) {
		t.Errorf("pulls = %v, want %v", got, want)
	}
	drawer := e.do("GET", "/partials/activity", nil, htmx).Body.String()
	for _, want := range []string{"check for updates and pull", "Pulling nginx:1.28 (newest minor version of nginx:1.27)", "Pulled 2 images."} {
		if !strings.Contains(drawer, want) {
			t.Errorf("activity misses %q:\n%s", want, drawer)
		}
	}
}

func TestImageUpdateDisabled(t *testing.T) {
	e := newEnv(t)
	e.login()
	if strings.Contains(e.do("GET", "/partials/containers", nil, htmx).Body.String(), "update-tag") {
		t.Error("update tag shown without update checks")
	}
	if strings.Contains(e.do("GET", "/partials/images", nil, htmx).Body.String(), "Check for updates") {
		t.Error("check button shown without update checks")
	}
	e.post("/containers/c0ffee000001/update", url.Values{"image": {"nginx:1.28"}}, htmx)
	e.jobs.Wait()
	if slices.ContainsFunc(e.rt.Calls, func(c string) bool { return strings.HasPrefix(c, "create") }) {
		t.Error("container updated without update checks")
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

// Recreate keeps the settings and applies the container's own pull
// policy: always pulls the image first, missing uses the local one.
func TestRecreateContainer(t *testing.T) {
	e := newEnv(t)
	e.login()
	pulled := func(ref string) bool { return slices.Contains(e.rt.Calls, "pull "+ref) }

	// web has no recorded policy: missing.
	w := e.post("/containers/c0ffee000001/recreate", nil, htmx)
	if w.Header().Get("HX-Redirect") != "/containers/web" {
		t.Fatalf("recreate redirect = %q", w.Header().Get("HX-Redirect"))
	}
	e.jobs.Wait()
	c := e.container("web")
	if c == nil || c.ID == "c0ffee000001" || c.Image != "nginx:1.27" || c.State != runtime.StateRunning || pulled("nginx:1.27") {
		t.Fatalf("recreated web = %+v, calls %v", c, e.rt.Calls)
	}
	if got := e.rt.LastCreate; got.Ports[0].HostPort != 8080 || !slices.Contains(got.Labels, "containerui.pull=missing") {
		t.Errorf("recreate opts = %+v", got)
	}

	// A container created with pull policy always is pulled again.
	form := url.Values{"image": {"alpine:latest"}, "name": {"pinger"}, "pull": {"always"}, "tty": {"on"}, "interactive": {"on"}}
	if w := e.post("/containers", form); w.Code != http.StatusSeeOther {
		t.Fatalf("create = %d\n%s", w.Code, w.Body.String())
	}
	list := e.do("GET", "/partials/containers", nil, htmx).Body.String()
	if !strings.Contains(list, "Pull policy always: alpine:latest is pulled first.") {
		t.Error("recreate confirmation does not name the pull policy")
	}
	e.rt.Calls = nil
	e.post("/containers/pinger/recreate", nil, htmx)
	e.jobs.Wait()
	if !pulled("alpine:latest") || !slices.Contains(e.rt.LastCreate.Labels, "containerui.pull=always") {
		t.Errorf("recreate with pull policy always: calls %v, labels %v", e.rt.Calls, e.rt.LastCreate.Labels)
	}
}

// The ⋮ menu of a container offers what fits its state.
func TestContainerMenuByState(t *testing.T) {
	e := newEnv(t)
	e.login()
	list := e.do("GET", "/partials/containers", nil, htmx).Body.String()
	menu := func(id string) string {
		i := strings.Index(list, `data-menu="c-`+id+`"`)
		if i < 0 {
			t.Fatalf("no menu for %s", id)
		}
		j := strings.Index(list[i:], "</details>")
		return list[i : i+j]
	}
	for id, c := range map[string]struct{ has, lacks []string }{
		"c0ffee000001": { // web, running
			has:   []string{">Stop<", ">Restart<", ">Edit<", ">Recreate<", ">Connect<", ">Watch logs<", ">Download logs<", ">Delete<", `name="force" value="true"`},
			lacks: []string{">Start<", ">Update…<"},
		},
		"c0ffee000002": { // job, stopped
			has:   []string{">Start<", ">Edit<", ">Recreate<", ">Download logs<", ">Delete<"},
			lacks: []string{">Stop<", ">Restart<", ">Connect<", ">Watch logs<"},
		},
	} {
		m := menu(id)
		for _, want := range c.has {
			if !strings.Contains(m, want) {
				t.Errorf("%s menu lacks %q", id, want)
			}
		}
		for _, bad := range c.lacks {
			if strings.Contains(m, bad) {
				t.Errorf("%s menu offers %q", id, bad)
			}
		}
	}
	if m := menu(e.container("shop-web-1").ID); strings.Contains(m, ">Edit<") || strings.Contains(m, ">Recreate<") || !strings.Contains(m, ">Delete<") {
		t.Errorf("compose container menu:\n%s", m)
	}
}

// A container page loaded while a recreate renames the old container away
// (listed, but gone by inspect) shows the job, not "not found".
func TestContainerDetailDuringRecreate(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.rt.InspectErr = func(id string) error {
		if id == "web" {
			return &runtime.CommandError{Op: "container inspect", Output: "container web not found", Kind: runtime.ErrNotFound}
		}
		return nil
	}
	notFound := func() bool {
		body := e.do("GET", "/containers/web", nil).Body.String()
		return strings.Contains(body, "container web not found")
	}
	if !notFound() {
		t.Error("inspect error without a recreate job is not shown")
	}

	release := make(chan struct{})
	e.jobs.Start(jobRecreate, "web", "admin", func(ctx context.Context, log func(string)) error {
		<-release
		return nil
	})
	if notFound() {
		t.Error("not found shown while the recreate job runs")
	}
	close(release)
	e.jobs.Wait()
	if !notFound() {
		t.Error("inspect error after the recreate job finished is not shown")
	}
}
