package web

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestNavOrder(t *testing.T) {
	e := newEnv(t)
	e.login()
	body := e.do("GET", "/", nil).Body.String()
	order := []string{`href="/" `, `href="/compose" `, `href="/containers" `, `href="/images" `, `href="/volumes" `, `href="/networks" `}
	last := -1
	for _, link := range order {
		i := strings.Index(body, link)
		if i < 0 || i < last {
			t.Fatalf("nav link %s missing or out of order", link)
		}
		last = i
	}
}

func TestNetworkPagesRender(t *testing.T) {
	e := newEnv(t)
	e.login()
	pages := map[string][]string{
		"/networks":                {"Create a Network", "Prune networks", `href="/networks/backend"`, "built-in", "macvlan", "--ipv6", `href="/containers/shop-web-1"`},
		"/partials/networks":       {`id="networks"`, "backend"},
		"/networks/backend":        {"172.30.0.0/16", "172.30.0.1", "team", "platform", "Connect a Container", "No containers are connected.", "Inspect", `action="/networks/delete"`},
		"/networks/bridge":         {"built-in", "web", "Disconnect", "Inspect"},
		"/networks/shop_default":   {"shop-web-1", "shop-db-1"},
		"/containers/c0ffee000001": {`<a href="/networks/bridge">bridge</a>`},
		"/":                        {`href="/networks/backend"`},
	}
	for path, want := range pages {
		w := e.do("GET", path, nil)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d", path, w.Code)
			continue
		}
		for _, s := range want {
			if !strings.Contains(w.Body.String(), s) {
				t.Errorf("GET %s: missing %q", path, s)
			}
		}
	}
	// Built-in networks offer no delete.
	if body := e.do("GET", "/networks/bridge", nil).Body.String(); strings.Contains(body, `action="/networks/delete"`) {
		t.Error("bridge offers Delete")
	}
	if w := e.do("GET", "/networks/nope", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown network = %d", w.Code)
	}
}

func TestNetworkCreate(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.post("/networks", url.Values{"name": {"web"}, "subnet": {"10.9.0.0/16"}, "gateway": {"10.1.0.1"}, "labels": {"a=b"}})
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "is not in subnet") ||
		!strings.Contains(w.Body.String(), `value="10.9.0.0/16"`) || !regexpOpen(w.Body.String()) {
		t.Errorf("invalid create = %d\n%s", w.Code, w.Body.String())
	}
	w = e.post("/networks", url.Values{"name": {"front"}, "driver": {"bridge"}, "subnet": {"10.9.0.0/16"}, "gateway": {"10.9.0.1"}, "internal": {"on"}, "options": {"mtu=1400\n"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/networks" {
		t.Fatalf("create = %d %s", w.Code, w.Header().Get("Location"))
	}
	body := e.do("GET", "/networks", nil).Body.String()
	if !strings.Contains(body, "Network front created.") || !strings.Contains(body, `href="/networks/front"`) {
		t.Errorf("list after create:\n%s", body)
	}
	if body := e.do("GET", "/networks/front", nil).Body.String(); !strings.Contains(body, "10.9.0.0/16") || !strings.Contains(body, "mtu") {
		t.Errorf("detail after create:\n%s", body)
	}
}

// regexpOpen reports whether the advanced section is rendered open.
func regexpOpen(body string) bool {
	m := advancedOpenRe.FindStringSubmatch(body)
	return m != nil && m[1] == " open"
}

func TestNetworksHiddenWithoutSupport(t *testing.T) {
	e := newEnv(t)
	e.rt.NoNetworks = true
	e.login()
	if strings.Contains(e.do("GET", "/", nil).Body.String(), `href="/networks"`) {
		t.Error("nav links to networks")
	}
	for _, path := range []string{"/networks", "/partials/networks", "/networks/bridge"} {
		if w := e.do("GET", path, nil); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d", path, w.Code)
		}
	}
	for _, path := range []string{"/networks", "/networks/delete", "/networks/prune", "/networks/bridge/connect", "/networks/bridge/disconnect"} {
		if w := e.post(path, url.Values{"name": {"x"}, "container": {"web"}}); w.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d", path, w.Code)
		}
	}
}

func TestNetworkDeleteAndPrune(t *testing.T) {
	e := newEnv(t)
	e.login()
	// In use: the conflict hint, and the network stays.
	w := e.post("/networks/delete", url.Values{"name": {"shop_default"}}, htmx)
	if !strings.Contains(w.Body.String(), "disconnect them first") {
		t.Errorf("delete in use: %s", w.Body.String())
	}
	w = e.post("/networks/delete", url.Values{"name": {"backend"}, "view": {"detail"}}, htmx)
	if w.Header().Get("HX-Redirect") != "/networks" {
		t.Errorf("delete from detail = %v", w.Header())
	}
	if w := e.do("GET", "/networks/backend", nil); w.Code != http.StatusNotFound {
		t.Errorf("deleted network still there: %d", w.Code)
	}
	if _ = e.post("/networks", url.Values{"name": {"spare"}}); !slices.Contains(e.rt.Calls, "create-network spare") {
		t.Fatal("create spare")
	}
	w = e.post("/networks/prune", nil, htmx)
	if body := w.Body.String(); !strings.Contains(body, "Pruned unused networks. Deleted Networks: spare") || strings.Contains(body, `href="/networks/spare"`) {
		t.Errorf("prune: %s", body)
	}
	if !strings.Contains(e.post("/networks/prune", nil, htmx).Body.String(), "No unused networks to prune.") {
		t.Error("second prune")
	}
}

func TestNetworkConnectDisconnect(t *testing.T) {
	e := newEnv(t)
	e.login()
	w := e.post("/networks/backend/connect", url.Values{"container": {"web"}, "aliases": {"www, frontend"}, "ip": {"172.30.0.10"}}, htmx)
	if w.Header().Get("HX-Redirect") != "/networks/backend" {
		t.Fatalf("connect = %d %v", w.Code, w.Header())
	}
	body := e.do("GET", "/networks/backend", nil).Body.String()
	for _, want := range []string{"Connected web to network backend.", "172.30.0.10", "www, frontend", "Disconnect"} {
		if !strings.Contains(body, want) {
			t.Errorf("after connect: missing %q", want)
		}
	}
	// Bad input is reported, not passed on.
	e.post("/networks/backend/connect", url.Values{"container": {"job"}, "ip": {"nope"}}, htmx)
	if body := e.do("GET", "/networks/backend", nil).Body.String(); !strings.Contains(body, "is not an IPv4 address") {
		t.Error("invalid IP not reported")
	}
	e.post("/networks/backend/disconnect", url.Values{"container": {"web"}}, htmx)
	if body := e.do("GET", "/networks/backend", nil).Body.String(); !strings.Contains(body, "Disconnected web from network backend.") || !strings.Contains(body, "No containers are connected.") {
		t.Errorf("after disconnect:\n%s", body)
	}
	want := []string{"connect backend web", "disconnect backend web"}
	var got []string
	for _, c := range e.rt.Calls {
		if strings.HasPrefix(c, "connect ") || strings.HasPrefix(c, "disconnect ") {
			got = append(got, c)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}
