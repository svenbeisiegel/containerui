package web

import (
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

var advancedOpenRe = regexp.MustCompile(`<details class="section advanced"( open)?>`)

// checkbox reports whether the named checkbox is rendered checked.
func checkbox(body, name string) bool {
	return regexp.MustCompile(`name="` + name + `" checked`).MatchString(body)
}

func TestCreateFormAdvancedDefaults(t *testing.T) {
	e := newEnv(t)
	e.login()
	body := e.do("GET", "/containers/new", nil).Body.String()
	m := advancedOpenRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("advanced section missing")
	}
	if m[1] != "" {
		t.Error("advanced section must be collapsed by default")
	}
	for name, want := range map[string]bool{"tty": true, "interactive": true, "rm": false, "no_healthcheck": false} {
		if got := checkbox(body, name); got != want {
			t.Errorf("checkbox %s checked = %v, want %v", name, got, want)
		}
	}
	for _, name := range []string{"cpus", "memory", "hostname", "user", "dns", "dns_search", "dns_option", "ulimit"} {
		if !strings.Contains(body, `name="`+name+`"`) {
			t.Errorf("field %s missing", name)
		}
	}
}

func TestCreateWithAdvancedOptions(t *testing.T) {
	e := newEnv(t)
	e.login()
	form := url.Values{
		"image": {"alpine:latest"}, "name": {"adv"}, "cmd": {"sleep 300"}, "pull": {"missing"},
		"tty": {"on"}, "interactive": {"on"}, "rm": {"on"}, "no_healthcheck": {"on"},
		"cpus": {"0.5"}, "memory": {" 64M "}, "hostname": {"cui-host"}, "user": {"1000:1000"},
		"dns": {"1.1.1.1, 8.8.8.8"}, "dns_search": {"example.com"}, "dns_option": {"ndots:2 timeout:3"},
		"ulimit": {"nofile=1024:2048\nnproc=512"},
	}
	if w := e.post("/containers", form); w.Code != http.StatusSeeOther {
		t.Fatalf("create = %d\n%s", w.Code, w.Body.String())
	}
	got := e.rt.LastCreate
	want := runtime.CreateContainerOpts{
		Image: "alpine:latest", Name: "adv", Cmd: []string{"sleep", "300"}, Pull: runtime.PullMissing,
		TTY: true, Interactive: true, AutoRemove: true, NoHealthcheck: true,
		CPUs: "0.5", Memory: "64M", Hostname: "cui-host", User: "1000:1000",
		DNS: []string{"1.1.1.1", "8.8.8.8"}, DNSSearch: []string{"example.com"},
		DNSOptions: []string{"ndots:2", "timeout:3"}, Ulimits: []string{"nofile=1024:2048", "nproc=512"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("opts =\n %+v\nwant\n %+v", got, want)
	}
}

func TestCreateUncheckedSwitchesAreFalse(t *testing.T) {
	e := newEnv(t)
	e.login()
	// A browser sends nothing for unchecked boxes.
	form := url.Values{"image": {"alpine:latest"}, "name": {"plain"}, "pull": {"missing"}}
	if w := e.post("/containers", form); w.Code != http.StatusSeeOther {
		t.Fatalf("create = %d", w.Code)
	}
	if o := e.rt.LastCreate; o.TTY || o.Interactive || o.AutoRemove || o.NoHealthcheck || o.Start {
		t.Errorf("unchecked switches passed as true: %+v", o)
	}
}

func TestCreateAdvancedErrorReopensSection(t *testing.T) {
	e := newEnv(t)
	e.login()
	form := url.Values{
		"image": {"alpine:latest"}, "name": {"bad"}, "pull": {"missing"},
		"tty": {"on"}, "interactive": {"on"}, "memory": {"12x"}, "ulimit": {"bogus=1"}, "hostname": {"keep-me"},
	}
	w := e.post("/containers", form)
	body := w.Body.String()
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d", w.Code)
	}
	if m := advancedOpenRe.FindStringSubmatch(body); m == nil || m[1] != " open" {
		t.Error("advanced section should be open after an error in it")
	}
	for _, want := range []string{"memory &#34;12x&#34;", "unknown ulimit", `value="keep-me"`, `value="12x"`, "bogus=1</textarea>"} {
		if !strings.Contains(body, want) {
			t.Errorf("re-rendered form missing %q", want)
		}
	}
	if !checkbox(body, "tty") || checkbox(body, "rm") {
		t.Error("switch states not preserved")
	}
}
