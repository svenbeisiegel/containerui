package containerui

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The notices must list every Go module linked into the binary, at the
// version that is linked, for each platform the binary is built for.
func TestNoticesListLinkedModules(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command to list the modules with")
	}
	for _, goos := range []string{"linux", "windows"} {
		cmd := exec.Command("go", "list", "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}", "./cmd/containerui")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "CGO_ENABLED=0")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list for %s: %v", goos, err)
		}
		seen := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line == "" || seen[line] {
				continue
			}
			seen[line] = true
			path, version, _ := strings.Cut(line, " ")
			if !regexp.MustCompile(`\| ` + regexp.QuoteMeta(path) + `( \([^)]*\))? \| ` + regexp.QuoteMeta(version) + ` \|`).MatchString(Notices) {
				t.Errorf("THIRD_PARTY_NOTICES.md does not list %s %s, linked on %s: add it with its license", path, version, goos)
			}
		}
	}
}

// The notices name the Go release the binaries are built with, which is the
// go directive in go.mod (the release workflow builds with it).
func TestNoticesNameGoRelease(t *testing.T) {
	mod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^go (\S+)`).FindSubmatch(mod)
	if m == nil {
		t.Fatal("no go directive in go.mod")
	}
	if want := "| Go standard library and runtime | go" + string(m[1]) + " |"; !strings.Contains(Notices, want) {
		t.Errorf("THIRD_PARTY_NOTICES.md does not name go%s, the Go release in go.mod", m[1])
	}
}

// The notices name the htmx release that is embedded.
func TestNoticesNameHtmxRelease(t *testing.T) {
	js, err := os.ReadFile("internal/web/static/htmx.min.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`version:"([0-9.]+)"`).FindSubmatch(js)
	if m == nil {
		t.Fatal("no version in htmx.min.js")
	}
	if want := "| htmx | " + string(m[1]) + " |"; !strings.Contains(Notices, want) {
		t.Errorf("THIRD_PARTY_NOTICES.md does not name htmx %s, the embedded release", m[1])
	}
}
