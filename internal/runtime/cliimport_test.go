package runtime

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestImportFromCLI(t *testing.T) {
	cmd := `sudo docker run -dit --rm --name web \
  -p 8080:80 -p127.0.0.1:8443:443/tcp --publish=9000-9001:9000-9001 \
  -v /srv/www:/usr/share/nginx/html:ro -v data:/data \
  --mount type=bind,source=/etc/app,target=/etc/app,readonly \
  -e TZ=Europe/Berlin --env "GREETING=hello world" -e SECRET \
  -l team=web --label=traefik.enable=true \
  --network backend --network-alias api --ip 172.30.0.10 \
  --restart unless-stopped --entrypoint /docker-entrypoint.sh -w /app -u 1000:1000 -h web01 \
  --cpus 1.5 -m 512mb --dns 1.1.1.1 --dns-search corp.example --dns-opt ndots:2 \
  --ulimit nofile=1024:2048 --add-host db=10.0.0.5 --cap-add NET_ADMIN --cap-drop ALL \
  --privileged --no-healthcheck --pull always \
  --device /dev/ttyUSB0 --init --tmpfs /run -P \
  nginx:1.27 nginx -g 'daemon off;'`
	imp, err := ImportFromCLI(cmd)
	if err != nil {
		t.Fatal(err)
	}
	o := imp.Opts
	want := CreateContainerOpts{
		Image: "nginx:1.27", Name: "web", Cmd: []string{"nginx", "-g", "daemon off;"},
		Env: []string{"TZ=Europe/Berlin", "GREETING=hello world", "SECRET"},
		Ports: []PortMapping{
			{HostPort: 8080, ContainerPort: 80},
			{HostIP: "127.0.0.1", HostPort: 8443, ContainerPort: 443, Protocol: "tcp"},
			{HostPort: 9000, ContainerPort: 9000}, {HostPort: 9001, ContainerPort: 9001},
		},
		Volumes: []VolumeMount{
			{Source: "/srv/www", Target: "/usr/share/nginx/html", ReadOnly: true},
			{Source: "data", Target: "/data"},
			{Source: "/etc/app", Target: "/etc/app", ReadOnly: true},
		},
		Networks: []NetworkAttachment{{Name: "backend", Aliases: []string{"api"}, IP: "172.30.0.10"}},
		Pull:     PullAlways, Start: true,
		TTY: true, Interactive: true, AutoRemove: true, NoHealthcheck: true,
		CPUs: "1.5", Memory: "512m", Hostname: "web01", User: "1000:1000",
		DNS: []string{"1.1.1.1"}, DNSSearch: []string{"corp.example"}, DNSOptions: []string{"ndots:2"},
		Ulimits:       []string{"nofile=1024:2048"},
		RestartPolicy: "unless-stopped", Labels: []string{"team=web", "traefik.enable=true"},
		Entrypoint: []string{"/docker-entrypoint.sh"}, WorkDir: "/app", ExtraHosts: []string{"db:10.0.0.5"},
		CapAdd: []string{"NET_ADMIN"}, CapDrop: []string{"ALL"}, Privileged: true,
	}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("opts =\n%+v\nwant\n%+v", o, want)
	}
	if wantLost := []string{"--device /dev/ttyUSB0", "--init", "--tmpfs /run", "-P"}; !slices.Equal(imp.Lost, wantLost) {
		t.Errorf("lost = %q, want %q", imp.Lost, wantLost)
	}
	if len(imp.Notes) != 1 || !strings.Contains(imp.Notes[0], "SECRET") {
		t.Errorf("notes = %q", imp.Notes)
	}
}

func TestImportFromCLIForms(t *testing.T) {
	for name, tc := range map[string]struct {
		in    string
		check func(t *testing.T, imp ImportSource)
	}{
		"create does not start": {"docker create alpine", func(t *testing.T, imp ImportSource) {
			if imp.Opts.Start || imp.Opts.TTY || imp.Opts.Interactive || imp.Opts.Image != "alpine" || imp.Opts.Cmd != nil {
				t.Errorf("opts = %+v", imp.Opts)
			}
		}},
		"container run, podman, prompt": {"$ podman container run -t docker.io/library/redis:7", func(t *testing.T, imp ImportSource) {
			if !imp.Opts.Start || !imp.Opts.TTY || imp.Opts.Image != "docker.io/library/redis:7" {
				t.Errorf("opts = %+v", imp.Opts)
			}
		}},
		"powershell continuation, windows path": {"docker run `\r\n  -v C:\\data:/data `\r\n  -p 80:80 nginx", func(t *testing.T, imp ImportSource) {
			if !reflect.DeepEqual(imp.Opts.Volumes, []VolumeMount{{Source: `C:\data`, Target: "/data"}}) || len(imp.Opts.Ports) != 1 {
				t.Errorf("opts = %+v", imp.Opts)
			}
		}},
		"cmd continuation": {"docker run ^\n --name=x ^\n busybox echo hi", func(t *testing.T, imp ImportSource) {
			if imp.Opts.Name != "x" || !slices.Equal(imp.Opts.Cmd, []string{"echo", "hi"}) {
				t.Errorf("opts = %+v", imp.Opts)
			}
		}},
		"other commands around it": {"docker pull nginx && docker run -d --rm=false nginx\n# done", func(t *testing.T, imp ImportSource) {
			if imp.Opts.Image != "nginx" || imp.Opts.AutoRemove {
				t.Errorf("opts = %+v", imp.Opts)
			}
		}},
		"forgotten continuation": {"docker run\n  -p 80:80 nginx", func(t *testing.T, imp ImportSource) {
			if imp.Opts.Image != "nginx" || len(imp.Opts.Ports) != 1 {
				t.Errorf("opts = %+v", imp.Opts)
			}
		}},
		"double dash, global flags": {"docker --context prod run --name a -- alpine:3 sh", func(t *testing.T, imp ImportSource) {
			if imp.Opts.Image != "alpine:3" || !slices.Equal(imp.Opts.Cmd, []string{"sh"}) || imp.Opts.Name != "a" {
				t.Errorf("opts = %+v", imp.Opts)
			}
		}},
		"network long form and default": {"docker run --network name=front,alias=www --net bridge --network-alias x nginx", func(t *testing.T, imp ImportSource) {
			want := []NetworkAttachment{{Name: "front", Aliases: []string{"www", "x"}}}
			if !reflect.DeepEqual(imp.Opts.Networks, want) {
				t.Errorf("networks = %+v", imp.Opts.Networks)
			}
		}},
		"aliases without network": {"docker run --network-alias x nginx", func(t *testing.T, imp ImportSource) {
			if len(imp.Lost) != 1 || !strings.Contains(imp.Lost[0], "network aliases x") {
				t.Errorf("lost = %q", imp.Lost)
			}
		}},
		"anonymous volume and tmpfs mount": {"docker run -v /data --mount type=tmpfs,target=/tmp -v cache:/c:z nginx", func(t *testing.T, imp ImportSource) {
			if len(imp.Lost) != 3 || len(imp.Opts.Volumes) != 1 {
				t.Errorf("lost = %q, volumes = %+v", imp.Lost, imp.Opts.Volumes)
			}
		}},
		"huge port range": {"docker run -p 1000-3000:1000-3000 nginx", func(t *testing.T, imp ImportSource) {
			if len(imp.Opts.Ports) != 0 || len(imp.Lost) != 1 {
				t.Errorf("ports = %+v, lost = %q", imp.Opts.Ports, imp.Lost)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			imp, err := ImportFromCLI(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, imp)
		})
	}
}

func TestImportFromCLIRejected(t *testing.T) {
	for in, want := range map[string]string{
		"":                                    "paste a docker run",
		"ls -la":                              "paste a docker run",
		"docker ps -a":                        "paste a docker run",
		"docker compose up -d":                "compose file",
		"docker run -d -p 80:80":              "names no image",
		"docker run --name":                   "--name needs a value",
		"docker run -e 'A=b nginx":            "unterminated ' quote",
		"docker run a; docker create b":       "contains 2 docker run",
		"docker run --rm=maybe nginx":         "expected true or false",
		`docker run -e "A=\"b\" nginx`:        "unterminated \" quote",
		"docker run --network container:x -h": "-h needs a value",
	} {
		_, err := ImportFromCLI(in)
		if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", in, err, want)
		}
	}
}
