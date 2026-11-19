package compose

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

const shopCompose = `name: shop
x-common: &common
  restart: unless-stopped
  environment:
    TZ: Europe/Berlin
services:
  web:
    <<: *common
    image: nginx:${NGINX_TAG:-1.27}
    container_name: shop-web
    command: ["nginx", "-g", "daemon off;"]
    ports:
      - "8080:80"
      - target: 443
        published: "8443"
        host_ip: 127.0.0.1
        protocol: tcp
      - "9000-9001:9000-9001"
    volumes:
      - static:/usr/share/nginx/html:ro
      - ./conf:/etc/nginx/conf.d
      - type: bind
        source: /srv/certs
        target: /certs
        read_only: true
      - type: tmpfs
        target: /cache
      - shared:/shared
    networks:
      default:
      backend:
        aliases: [www]
        ipv4_address: 172.30.0.10
    labels:
      traefik.enable: "true"
    environment:
      APP_ENV: production
      SECRET:
      API_KEY: ${API_KEY}
    depends_on: [db]
    healthcheck:
      disable: true
    ulimits:
      nproc: 512
      nofile: {soft: 1024, hard: 2048}
    deploy:
      resources:
        limits: {cpus: "0.5", memory: 256MB}
    extra_hosts: ["db.local=10.0.0.5"]
    logging: {driver: json-file}
    stop_signal: SIGQUIT
    tty: true
  db:
    image: postgres:16
    command: postgres -c "max_connections=200"
  builder:
    build: .
volumes:
  static:
  shared:
    external: true
networks:
  backend:
`

func TestImportComposeChoices(t *testing.T) {
	imp, choices, err := ImportCompose([]byte(shopCompose), "")
	if err != nil {
		t.Fatal(err)
	}
	want := []Service{{Name: "web", Image: "nginx:1.27", Replicas: 1}, {Name: "db", Image: "postgres:16", Replicas: 1}}
	if !reflect.DeepEqual(choices, want) || imp.Opts.Image != "" {
		t.Errorf("choices = %+v, imp = %+v", choices, imp)
	}

	imp, choices, err = ImportCompose([]byte(shopCompose), "db")
	if err != nil || choices != nil {
		t.Fatal(err, choices)
	}
	if !slices.Equal(imp.Opts.Cmd, []string{"postgres", "-c", "max_connections=200"}) || imp.Opts.Name != "db" || imp.Project != "shop" || imp.Service != "db" {
		t.Errorf("db = %+v", imp)
	}

	if _, _, err := ImportCompose([]byte(shopCompose), "builder"); err == nil || !strings.Contains(err.Error(), "build the image first") {
		t.Errorf("builder: %v", err)
	}
	if _, _, err := ImportCompose([]byte(shopCompose), "nope"); err == nil || !strings.Contains(err.Error(), `no service "nope"`) {
		t.Errorf("nope: %v", err)
	}
}

func TestImportComposeService(t *testing.T) {
	imp, _, err := ImportCompose([]byte(shopCompose), "web")
	if err != nil {
		t.Fatal(err)
	}
	want := runtime.CreateContainerOpts{
		Image: "nginx:1.27", Name: "shop-web", Cmd: []string{"nginx", "-g", "daemon off;"},
		Env: []string{"APP_ENV=production", "SECRET", "API_KEY=${API_KEY}"},
		Ports: []runtime.PortMapping{
			{HostPort: 8080, ContainerPort: 80},
			{HostIP: "127.0.0.1", HostPort: 8443, ContainerPort: 443, Protocol: "tcp"},
			{HostPort: 9000, ContainerPort: 9000}, {HostPort: 9001, ContainerPort: 9001},
		},
		Volumes: []runtime.VolumeMount{
			{Source: "shop_static", Target: "/usr/share/nginx/html", ReadOnly: true},
			{Source: "./conf", Target: "/etc/nginx/conf.d"},
			{Source: "/srv/certs", Target: "/certs", ReadOnly: true},
			{Source: "shared", Target: "/shared"},
		},
		Networks: []runtime.NetworkAttachment{
			{Name: "shop_default"},
			{Name: "shop_backend", Aliases: []string{"www"}, IP: "172.30.0.10"},
		},
		Pull: runtime.PullMissing, Start: true, TTY: true, NoHealthcheck: true,
		CPUs: "0.5", Memory: "256M",
		Ulimits:       []string{"nproc=512", "nofile=1024:2048"},
		RestartPolicy: "unless-stopped", Labels: []string{"traefik.enable=true"},
		ExtraHosts: []string{"db.local:10.0.0.5"},
	}
	if !reflect.DeepEqual(imp.Opts, want) {
		t.Errorf("opts =\n%+v\nwant\n%+v", imp.Opts, want)
	}
	if wantLost := []string{"tmpfs mount at /cache", "logging", "stop_signal: SIGQUIT"}; !slices.Equal(imp.Lost, wantLost) {
		t.Errorf("lost = %q", imp.Lost)
	}
	notes := strings.Join(imp.Notes, "\n")
	for _, w := range []string{"./conf is relative", "Volume static is named shop_static", "Network backend is named shop_backend",
		"shop_default, the default network", "SECRET takes its value", "depends on db", "uses API_KEY"} {
		if !strings.Contains(notes, w) {
			t.Errorf("notes miss %q:\n%s", w, notes)
		}
	}
}

func TestImportComposeFragments(t *testing.T) {
	for name, tc := range map[string]struct {
		in, image, cname string
	}{
		"service block copied from the middle": {"  web:\n    image: nginx\n    ports:\n      - 80:80\n", "nginx", "web"},
		"bare service body":                    {"image: redis:7\nrestart: always\n", "redis:7", ""},
		"indented body":                        {"    image: redis:7\n    command: redis-server --save ''\n", "redis:7", ""},
		"services key without the rest":        {"services:\n  api:\n    image: ghcr.io/acme/api:2\n", "ghcr.io/acme/api:2", "api"},
		"one image among build services":       {"services:\n  app:\n    build: .\n  cache:\n    image: redis\n", "redis", "cache"},
		"fragment with top-level volumes":      {"web:\n  image: nginx\n  volumes: [data:/d]\nvolumes:\n  data:\n", "nginx", "web"},
	} {
		t.Run(name, func(t *testing.T) {
			imp, choices, err := ImportCompose([]byte(tc.in), "")
			if err != nil || choices != nil {
				t.Fatal(err, choices)
			}
			if imp.Opts.Image != tc.image || imp.Opts.Name != tc.cname {
				t.Errorf("opts = %+v", imp.Opts)
			}
		})
	}
	imp, _, _ := ImportCompose([]byte("    image: redis:7\n    command: redis-server --save ''\n"), "")
	if !slices.Equal(imp.Opts.Cmd, []string{"redis-server", "--save", ""}) {
		t.Errorf("cmd = %q", imp.Opts.Cmd)
	}
	imp, _, _ = ImportCompose([]byte("web:\n  image: nginx\n  volumes: [data:/d]\n  network_mode: host\n"), "")
	if imp.Opts.Volumes[0].Source != "data" || !strings.Contains(strings.Join(imp.Notes, " "), "myapp_data") ||
		!reflect.DeepEqual(imp.Opts.Networks, []runtime.NetworkAttachment{{Name: "host"}}) {
		t.Errorf("unknown project: %+v %q", imp.Opts, imp.Notes)
	}
}

func TestImportComposeRejected(t *testing.T) {
	for in, want := range map[string]string{
		"":                                "paste a compose file",
		"just text":                       "must be a mapping",
		"web:\n  image: [\n":              "not valid YAML",
		"foo: bar\n":                      "no service found",
		"services: []\n":                  "services: must be a mapping",
		"services:\n  app:\n    build: .": "no image: build: is not supported",
		"services:\n  a:\n    build: .\n  b:\n    build: .\n": "no service of the compose file names an image",
	} {
		_, _, err := ImportCompose([]byte(in), "")
		if err == nil || !errors.Is(err, runtime.ErrInvalid) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", in, err, want)
		}
	}
}
