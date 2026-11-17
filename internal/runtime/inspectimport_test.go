package runtime

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func readInspectFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/inspect_compose_web.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseContainerInspect(t *testing.T) {
	fixture := readInspectFixture(t)
	var docs []json.RawMessage
	if err := json.Unmarshal(fixture, &docs); err != nil {
		t.Fatal(err)
	}
	one := string(docs[0])

	for name, in := range map[string]string{
		"docker array":     string(fixture),
		"object":           one,
		"BOM and spaces":   "\xef\xbb\xbf\n  " + one + "\n",
		"podman container": `[{"Id":"abc","Name":"web","Config":{"Image":"docker.io/library/nginx:1.27"},"HostConfig":{},"State":{"Status":"running"}}]`,
	} {
		doc, err := ParseContainerInspect([]byte(in))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var c ContainerInspect
		if err := json.Unmarshal(doc, &c); err != nil || c.ID == "" {
			t.Errorf("%s: document %s", name, doc)
		}
	}

	for name, tc := range map[string]struct{ in, want string }{
		"empty":            {"  ", "paste the output"},
		"not JSON":         {"docker inspect web", "not valid JSON"},
		"empty array":      {"[]", "is empty"},
		"two":              {"[" + one + "," + one + "]", "contains 2 objects"},
		"string":           {`"web"`, "not a JSON object"},
		"array of strings": {`["web"]`, "not a JSON object"},
		"image":            {`[{"Id":"sha256:1","RepoTags":["nginx:1.27"],"Config":{},"RootFS":{}}]`, "inspect output of an image"},
		"volume":           {`[{"Name":"data","Driver":"local","Mountpoint":"/var/lib/docker/volumes/data/_data"}]`, "of a volume"},
		"network":          {`[{"Name":"bridge","Id":"n1","IPAM":{}}]`, "of a network"},
		"no HostConfig":    {`{"Id":"abc","Config":{"Image":"nginx"},"State":{}}`, "not the inspect output of a container"},
		"no image":         {`{"Id":"abc","Config":{},"HostConfig":{},"State":{}}`, "names no image"},
		"no id":            {`{"Config":{"Image":"nginx"},"HostConfig":{},"State":{}}`, "has no Id"},
	} {
		_, err := ParseContainerInspect([]byte(tc.in))
		if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestImportFromInspect(t *testing.T) {
	doc, err := ParseContainerInspect(readInspectFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	image := json.RawMessage(`{"Id":"sha256:bbb222","Config":{"Env":["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"],` +
		`"Cmd":["nginx","-g","daemon off;"],"Entrypoint":["/docker-entrypoint.sh"],"StopSignal":"SIGQUIT"}}`)
	imp, err := ImportFromInspect(doc, image)
	if err != nil {
		t.Fatal(err)
	}
	o := imp.Opts
	if imp.Name != "shop-web-1" || imp.Project != "shop" || imp.Service != "web" || o.Name != "web" || o.Image != "nginx:1.27" || !o.Start || o.Pull != PullMissing {
		t.Errorf("import = %+v", imp)
	}
	if !slices.Equal(o.Env, []string{"APP_ENV=production", "TZ=Europe/Berlin"}) || o.Cmd != nil || o.Entrypoint != nil {
		t.Errorf("env %v cmd %v entrypoint %v: the image's defaults must be left out", o.Env, o.Cmd, o.Entrypoint)
	}
	if !slices.Equal(o.Labels, []string{"traefik.enable=true"}) {
		t.Errorf("labels = %v", o.Labels)
	}
	if len(o.Ports) != 1 || o.Ports[0].PortSpec() != "8082:80" {
		t.Errorf("ports = %+v", o.Ports)
	}
	var vols []string
	for _, v := range o.Volumes {
		vols = append(vols, v.String())
	}
	if !slices.Equal(vols, []string{"shop_static:/usr/share/nginx/html:ro", "/srv/shop/nginx.conf:/etc/nginx/nginx.conf:ro"}) {
		t.Errorf("volumes = %v", vols)
	}
	if !reflect.DeepEqual(o.Networks, []NetworkAttachment{{Name: "shop_default", Aliases: []string{"web"}}}) {
		t.Errorf("networks = %+v", o.Networks)
	}
	if o.RestartPolicy != "unless-stopped" || o.Memory != "256m" || o.CPUs != "0.5" || o.Hostname != "" {
		t.Errorf("restart %q memory %q cpus %q hostname %q", o.RestartPolicy, o.Memory, o.CPUs, o.Hostname)
	}
	if !slices.Equal(imp.Lost, []string{"security option no-new-privileges:true"}) {
		t.Errorf("lost = %v", imp.Lost)
	}
	notes := strings.Join(imp.Notes, "\n")
	for _, want := range []string{"compose project shop", "Network shop_default", "Volume shop_static", "container shop-web-1 still exists"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes miss %q:\n%s", want, notes)
		}
	}
	if strings.Contains(notes, "not available locally") {
		t.Error("image was given but reported missing")
	}

	// Without the image everything the container has is kept.
	imp, err = ImportFromInspect(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(imp.Opts.Env) != 3 || imp.Opts.Entrypoint == nil || !strings.Contains(strings.Join(imp.Notes, "\n"), "nginx:1.27 is not available locally") {
		t.Errorf("without image: env %v entrypoint %v notes %v", imp.Opts.Env, imp.Opts.Entrypoint, imp.Notes)
	}
}

func TestImportStandaloneContainer(t *testing.T) {
	doc, err := ParseContainerInspect([]byte(`{"Id":"abc123","Name":"/my app","Config":{"Image":"alpine","Hostname":"abc"},"HostConfig":{"NetworkMode":"bridge"},"State":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	imp, err := ImportFromInspect(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	// An invalid name is left for the user to choose.
	if imp.Opts.Name != "" || imp.Project != "" || imp.Service != "" || imp.Opts.Networks != nil {
		t.Errorf("import = %+v", imp)
	}
	if strings.Contains(strings.Join(imp.Notes, "\n"), "compose") {
		t.Errorf("notes = %v", imp.Notes)
	}
}
