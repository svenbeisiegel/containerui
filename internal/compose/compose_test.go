package compose

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

func TestParseValid(t *testing.T) {
	src := `name: shop
services:
  web:
    image: nginx:1.27
    ports: ["8080:80"]
    depends_on: [db]
    networks: [front, default]
    volumes:
      - ./html:/usr/share/nginx/html:ro
      - C:\data:/data
  db:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: secret
    volumes:
      - dbdata:/var/lib/postgresql/data
    deploy:
      replicas: 2
networks:
  front:
volumes:
  dbdata:
x-common: &common {}
`
	f, problems := Parse([]byte(src))
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	want := &File{Name: "shop", Services: []Service{{"web", "nginx:1.27", 1}, {"db", "postgres:16", 2}},
		Networks: []string{"front"}, Volumes: []string{"dbdata"}}
	if !reflect.DeepEqual(f, want) {
		t.Errorf("Parse = %+v", f)
	}
}

func TestParseProblems(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      []Problem
	}{
		{"empty", "  \n", []Problem{{Msg: "the file is empty"}}},
		{"syntax", "services:\n  web:\n    image: nginx\n   ports: x\n", []Problem{{Line: 4, Msg: "did not find expected key"}}},
		{"tab", "services:\n\tweb: {}\n", []Problem{{Line: 2, Msg: "found character that cannot start any token"}}},
		{"not a mapping", "- a\n- b\n", []Problem{{Line: 1, Col: 1, Msg: "the top level must be a mapping with a services: key"}}},
		{"no services", "name: x\n", []Problem{{Line: 1, Col: 1, Msg: "services: is missing"}}},
		{"version and unknown", "version: '3'\nservice:\n  a: {}\nservices:\n  web:\n    image: x\n", []Problem{
			{Line: 1, Col: 1, Msg: "version is obsolete and ignored by Compose v2", Warning: true},
			{Line: 2, Col: 1, Msg: `unknown top-level key "service"`},
		}},
		{"service shape", "services:\n  web:\n    ports: 80\n    environment: x\n  bad name: {image: x}\n  db: postgres\n", []Problem{
			{Line: 2, Col: 3, Msg: `service "web" needs image or build`},
			{Line: 3, Col: 12, Msg: `service "web": ports must be a list`},
			{Line: 4, Col: 18, Msg: `service "web": environment must be a list or a mapping`},
			{Line: 5, Col: 3, Msg: `service name "bad name": use letters, digits, '.', '-' and '_'`},
			{Line: 6, Col: 7, Msg: `service "db" must be a mapping`},
		}},
		{"references", "services:\n  web:\n    image: x\n    depends_on: [db]\n    networks: [front]\n    volumes: [data:/d]\n", []Problem{
			{Line: 4, Col: 18, Msg: `service "web" depends on undefined service "db"`},
			{Line: 5, Col: 16, Msg: `service "web" uses undefined network "front" (add it under networks:)`},
			{Line: 6, Col: 15, Msg: `service "web" uses undefined volume "data" (add it under volumes:)`},
		}},
		{"duplicate", "services:\n  web:\n    image: x\n    image: y\n", []Problem{{Line: 4, Col: 5, Msg: `duplicate key "image"`}}},
		{"bad name", "name: My App\nservices:\n  web: {image: x}\n", []Problem{{Line: 1, Col: 7, Msg: `name "My App": use lowercase letters, digits, '-' and '_'`}}},
		{"two documents", "services:\n  web: {image: x}\n---\nfoo: 1\n", []Problem{{Line: 3, Col: 1, Msg: "only one YAML document is allowed (remove the extra ---)"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := Parse([]byte(tc.src))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Parse problems:\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestHasErrors(t *testing.T) {
	if HasErrors([]Problem{{Msg: "w", Warning: true}}) || !HasErrors([]Problem{{Msg: "e"}}) {
		t.Error("HasErrors")
	}
}

func TestStore(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "compose")}
	if list, err := s.List(); err != nil || list != nil {
		t.Errorf("List on missing dir = %v, %v", list, err)
	}
	path, err := s.Write("shop", []byte("services: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(s.Dir, "shop", FileName) {
		t.Errorf("path = %q", path)
	}
	if b, err := s.Read("shop"); err != nil || string(b) != "services: {}\n" {
		t.Errorf("Read = %q, %v", b, err)
	}
	if _, err := s.Write("shop", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Read("shop"); string(b) != "v2" {
		t.Errorf("overwrite = %q", b)
	}
	if list, _ := s.List(); !reflect.DeepEqual(list, []string{"shop"}) {
		t.Errorf("List = %v", list)
	}

	tmp, cleanup, err := s.WriteTemp("new", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(tmp) != filepath.Join(s.Dir, "new") {
		t.Errorf("temp file %q not in the project folder", tmp)
	}
	cleanup()
	if _, err := os.Stat(filepath.Join(s.Dir, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Error("WriteTemp left the project folder behind")
	}

	if err := s.Delete("shop"); err != nil {
		t.Fatal(err)
	}
	if s.Has("shop") {
		t.Error("deleted project still stored")
	}
	if _, err := s.Read("shop"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Read deleted = %v", err)
	}

	for _, bad := range []string{"../x", "..", "a/b", `a\b`, "Shop", "", "-x"} {
		if _, err := s.Write(bad, nil); !errors.Is(err, runtime.ErrInvalid) {
			t.Errorf("Write(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestGroup(t *testing.T) {
	lbl := func(project, service, num string) map[string]string {
		return map[string]string{runtime.LabelProject: project, runtime.LabelService: service, runtime.LabelNumber: num,
			runtime.LabelConfigFiles: "/srv/" + project + "/compose.yaml", runtime.LabelWorkingDir: "/srv/" + project}
	}
	cs := []runtime.Container{
		{ID: "3", Name: "shop-web-2", State: runtime.StateExited, Labels: lbl("shop", "web", "2")},
		{ID: "1", Name: "shop-web-1", State: runtime.StateRunning, Labels: lbl("shop", "web", "1")},
		{ID: "2", Name: "shop-db-1", State: runtime.StateRunning, Labels: lbl("shop", "db", "1")},
		{ID: "4", Name: "plain", State: runtime.StateRunning},
	}
	vs := []runtime.Volume{{Name: "shop_data", Labels: map[string]string{runtime.LabelProject: "shop"}}, {Name: "other"}}
	ns := []runtime.Network{{Name: "shop_default", Labels: map[string]string{runtime.LabelProject: "shop"}}}
	ps := Group(cs, vs, ns, []string{"blog", "shop"})
	if len(ps) != 2 || ps[0].Name != "blog" || ps[1].Name != "shop" {
		t.Fatalf("projects = %+v", ps)
	}
	blog, shop := ps[0], ps[1]
	if blog.Deployed() || !blog.Stored || blog.Status() != "not deployed" {
		t.Errorf("blog = %+v", blog)
	}
	if !shop.Stored || shop.Running != 2 || shop.Total != 3 || shop.Status() != "partial" {
		t.Errorf("shop = %+v", shop)
	}
	if !reflect.DeepEqual(shop.ServiceNames(), []string{"db", "web"}) {
		t.Errorf("services = %v", shop.ServiceNames())
	}
	if web := shop.Services[1]; web.Containers[0].Name != "shop-web-1" || web.Containers[1].Name != "shop-web-2" {
		t.Errorf("replica order = %+v", web.Containers)
	}
	if len(shop.Volumes) != 1 || len(shop.Networks) != 1 || shop.WorkingDir != "/srv/shop" ||
		!reflect.DeepEqual(shop.ConfigFiles, []string{"/srv/shop/compose.yaml"}) {
		t.Errorf("shop resources = %+v", shop)
	}
	if p, ok := Find(ps, "shop"); !ok || p.Name != "shop" {
		t.Error("Find")
	}
}

func TestGenerate(t *testing.T) {
	read := func(name string) json.RawMessage {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var containers []json.RawMessage
	if err := json.Unmarshal(read("inspect_containers.json"), &containers); err != nil {
		t.Fatal(err)
	}
	images := map[string]json.RawMessage{"nginx:1.27": read("inspect_image_nginx.json")}
	got, err := Generate("shop", containers, images)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "generated.yaml")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		_ = os.WriteFile(golden, got, 0o644)
	}
	want := strings.ReplaceAll(string(read("generated.yaml")), "\r\n", "\n")
	if string(got) != want {
		t.Errorf("Generate:\n%s\nwant:\n%s", got, want)
	}
	// What is generated must pass our own checks.
	if _, problems := Parse(got); len(problems) != 0 {
		t.Errorf("generated file has problems: %v", problems)
	}

	if _, err := Generate("nope", containers, nil); err == nil {
		t.Error("Generate for a project without containers should fail")
	}
}
