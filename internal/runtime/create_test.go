package runtime

import (
	"errors"
	"testing"
)

func TestParsePortSpec(t *testing.T) {
	cases := []struct {
		in   string
		want PortMapping
	}{
		{"80", PortMapping{ContainerPort: 80}},
		{"8080:80", PortMapping{HostPort: 8080, ContainerPort: 80}},
		{"127.0.0.1:8080:80/udp", PortMapping{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "udp"}},
	}
	for _, c := range cases {
		got, err := ParsePortSpec(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParsePortSpec(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
		if got.PortSpec() != c.in {
			t.Errorf("PortSpec() = %q; want %q", got.PortSpec(), c.in)
		}
	}
	for _, bad := range []string{"", "x", "0", "70000", "1:2:3:4", "a:80"} {
		if _, err := ParsePortSpec(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParsePortSpec(%q) err = %v; want ErrInvalid", bad, err)
		}
	}
}

func TestParseVolumeSpec(t *testing.T) {
	cases := []struct {
		in   string
		want VolumeMount
	}{
		{"data:/var/lib/data", VolumeMount{Source: "data", Target: "/var/lib/data"}},
		{`C:\work:/work:ro`, VolumeMount{Source: `C:\work`, Target: "/work", ReadOnly: true}},
		{"vol:/x:rw", VolumeMount{Source: "vol", Target: "/x"}},
	}
	for _, c := range cases {
		got, err := ParseVolumeSpec(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseVolumeSpec(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "/only", "relative:path"} {
		if _, err := ParseVolumeSpec(bad); err == nil {
			t.Errorf("ParseVolumeSpec(%q) should fail", bad)
		}
	}
}

func TestCreateValidate(t *testing.T) {
	ok := CreateContainerOpts{
		Image: "docker.io/library/alpine:latest", Name: "web-1",
		Env:     []string{"A=1", "B_2="},
		Ports:   []PortMapping{{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
		Volumes: []VolumeMount{{Source: "data", Target: "/data"}},
		Network: "bridge", Pull: PullMissing,
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid opts rejected: %v", err)
	}
	bad := []CreateContainerOpts{
		{Image: "--privileged"},
		{Image: "alpine", Name: "-rm"},
		{Image: "alpine", Name: "has space"},
		{Image: "alpine", Env: []string{"1BAD=x"}},
		{Image: "alpine", Env: []string{"NOEQUALS"}},
		{Image: "alpine", Ports: []PortMapping{{ContainerPort: 80, Protocol: "sctp"}}},
		{Image: "alpine", Volumes: []VolumeMount{{Source: "-v", Target: "/x"}}},
		{Image: "alpine", Network: "--net=host"},
		{Image: "alpine", Pull: "sometimes"},
		{Image: "alpine:"},
		{Image: "a//b"},
	}
	for _, o := range bad {
		if err := o.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate(%+v) = %v; want ErrInvalid", o, err)
		}
	}
}

func TestSplitRef(t *testing.T) {
	cases := map[string][2]string{
		"alpine":                      {"alpine", "latest"},
		"alpine:3.20":                 {"alpine", "3.20"},
		"localhost:5000/team/app":     {"localhost:5000/team/app", "latest"},
		"localhost:5000/team/app:v1":  {"localhost:5000/team/app", "v1"},
		"ghcr.io/a/b@sha256:0123abcd": {"ghcr.io/a/b", ""},
	}
	for in, want := range cases {
		if r, tg := SplitRef(in); r != want[0] || tg != want[1] {
			t.Errorf("SplitRef(%q) = %q, %q; want %q", in, r, tg, want)
		}
	}
}

func TestImageRef(t *testing.T) {
	cases := map[Image]string{
		{ID: "sha256:abc", Repository: "<none>", Tag: "<none>"}: "sha256:abc",
		{ID: "x", Repository: "alpine", Tag: "latest"}:          "alpine:latest",
		{ID: "x", Repository: "alpine", Tag: "<none>"}:          "alpine",
	}
	for img, want := range cases {
		if got := img.Ref(); got != want {
			t.Errorf("Ref(%+v) = %q; want %q", img, got, want)
		}
	}
}

func TestCreateValidateAdvanced(t *testing.T) {
	base := func(mod func(*CreateContainerOpts)) CreateContainerOpts {
		o := CreateContainerOpts{Image: "alpine"}
		mod(&o)
		return o
	}
	good := map[string]CreateContainerOpts{
		"cpus 1.5":       base(func(o *CreateContainerOpts) { o.CPUs = "1.5" }),
		"cpus 2":         base(func(o *CreateContainerOpts) { o.CPUs = "2" }),
		"memory 512M":    base(func(o *CreateContainerOpts) { o.Memory = "512M" }),
		"memory 1.5g":    base(func(o *CreateContainerOpts) { o.Memory = "1.5g" }),
		"memory bytes":   base(func(o *CreateContainerOpts) { o.Memory = "67108864" }),
		"hostname":       base(func(o *CreateContainerOpts) { o.Hostname = "web-01.corp.example" }),
		"user name":      base(func(o *CreateContainerOpts) { o.User = "root" }),
		"user uid:gid":   base(func(o *CreateContainerOpts) { o.User = "1000:1000" }),
		"dns v4 and v6":  base(func(o *CreateContainerOpts) { o.DNS = []string{"1.1.1.1", "::1"} }),
		"dns search":     base(func(o *CreateContainerOpts) { o.DNSSearch = []string{"corp.example.com"} }),
		"dns options":    base(func(o *CreateContainerOpts) { o.DNSOptions = []string{"ndots:2", "rotate", "use-vc"} }),
		"ulimit range":   base(func(o *CreateContainerOpts) { o.Ulimits = []string{"nofile=1024:2048"} }),
		"ulimit soft":    base(func(o *CreateContainerOpts) { o.Ulimits = []string{"nproc=512"} }),
		"ulimit no hard": base(func(o *CreateContainerOpts) { o.Ulimits = []string{"core=0:-1", "memlock=-1"} }),
		"booleans": base(func(o *CreateContainerOpts) {
			o.TTY, o.Interactive, o.AutoRemove, o.NoHealthcheck = true, true, true, true
		}),
	}
	for name, o := range good {
		if err := o.Validate(); err != nil {
			t.Errorf("%s: rejected: %v", name, err)
		}
	}
	bad := map[string]CreateContainerOpts{
		"cpus 0":           base(func(o *CreateContainerOpts) { o.CPUs = "0" }),
		"cpus negative":    base(func(o *CreateContainerOpts) { o.CPUs = "-1" }),
		"cpus text":        base(func(o *CreateContainerOpts) { o.CPUs = "abc" }),
		"memory unit":      base(func(o *CreateContainerOpts) { o.Memory = "12x" }),
		"memory zero":      base(func(o *CreateContainerOpts) { o.Memory = "0M" }),
		"memory flag":      base(func(o *CreateContainerOpts) { o.Memory = "--privileged" }),
		"hostname dash":    base(func(o *CreateContainerOpts) { o.Hostname = "-bad" }),
		"hostname space":   base(func(o *CreateContainerOpts) { o.Hostname = "a b" }),
		"user space":       base(func(o *CreateContainerOpts) { o.User = "a b" }),
		"user flag":        base(func(o *CreateContainerOpts) { o.User = "-u" }),
		"dns not ip":       base(func(o *CreateContainerOpts) { o.DNS = []string{"foo"} }),
		"dns search bad":   base(func(o *CreateContainerOpts) { o.DNSSearch = []string{"bad_domain"} }),
		"dns option bad":   base(func(o *CreateContainerOpts) { o.DNSOptions = []string{"--x"} }),
		"ulimit order":     base(func(o *CreateContainerOpts) { o.Ulimits = []string{"nofile=2048:1024"} }),
		"ulimit unlimited": base(func(o *CreateContainerOpts) { o.Ulimits = []string{"nofile=-1:1024"} }),
		"ulimit unknown":   base(func(o *CreateContainerOpts) { o.Ulimits = []string{"bogus=1"} }),
		"ulimit flag":      base(func(o *CreateContainerOpts) { o.Ulimits = []string{"nofile=--x"} }),
		"ulimit format":    base(func(o *CreateContainerOpts) { o.Ulimits = []string{"nofile"} }),
	}
	for name, o := range bad {
		if err := o.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v; want ErrInvalid", name, err)
		}
	}
}

func TestCreateVolumeValidate(t *testing.T) {
	drivers := []string{"guest", "vhd"}
	good := []CreateVolumeOpts{
		{},
		{Name: "data", Driver: "guest"},
		{Name: "3f2a9c0e", Driver: "vhd", Options: []string{"size=1G"}, Labels: []string{"com.example/app=web", "empty="}},
	}
	for _, o := range good {
		if err := o.Validate(drivers); err != nil {
			t.Errorf("Validate(%+v) = %v", o, err)
		}
	}
	bad := []CreateVolumeOpts{
		{Name: "-x"},
		{Name: "a b"},
		{Driver: "local"},
		{Options: []string{"size"}},
		{Options: []string{"--opt=1"}},
		{Labels: []string{"=x"}},
	}
	for _, o := range bad {
		if err := o.Validate(drivers); !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate(%+v) = %v; want ErrInvalid", o, err)
		}
	}
	if err := ValidateVolumeName("--all"); !errors.Is(err, ErrInvalid) {
		t.Errorf("ValidateVolumeName(--all) = %v", err)
	}
}
