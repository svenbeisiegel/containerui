package registry

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrCredentialHelper means the credentials for a registry are kept by a
// docker credential helper, which ContainerUI does not call.
var ErrCredentialHelper = errors.New("the credentials for this registry are kept by a credential helper, which is not supported")

// Credentials looks up the login for a registry host. An empty user means
// anonymous access.
type Credentials func(registry string) (user, password string, err error)

// Anonymous never has credentials.
func Anonymous(string) (string, string, error) { return "", "", nil }

type authFile struct {
	Auths map[string]struct {
		Auth          string `json:"auth"`
		Username      string `json:"username"`
		Password      string `json:"password"`
		IdentityToken string `json:"identitytoken"`
	} `json:"auths"`
	CredsStore  string            `json:"credsStore"`
	CredHelpers map[string]string `json:"credHelpers"`
}

// DefaultAuthFiles lists the auth files to read: configured (if set),
// then $REGISTRY_AUTH_FILE, podman's and docker's files.
func DefaultAuthFiles(configured string) []string {
	var files []string
	add := func(p string) {
		if p != "" {
			files = append(files, p)
		}
	}
	add(configured)
	add(os.Getenv("REGISTRY_AUTH_FILE"))
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		add(filepath.Join(d, "containers", "auth.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".config", "containers", "auth.json"))
		dockerDir := os.Getenv("DOCKER_CONFIG")
		if dockerDir == "" {
			dockerDir = filepath.Join(home, ".docker")
		}
		add(filepath.Join(dockerDir, "config.json"))
	}
	return files
}

// FileCredentials reads logins from docker/podman auth files; the first
// file with an entry for a registry wins. Files are read on every lookup,
// so a later "docker login" is picked up.
func FileCredentials(files []string) Credentials {
	return func(registry string) (string, string, error) {
		keys := hostKeys(registry)
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var af authFile
			if err := json.Unmarshal(b, &af); err != nil {
				return "", "", fmt.Errorf("read %s: %w", f, err)
			}
			for _, k := range keys {
				if af.CredHelpers[k] != "" {
					return "", "", ErrCredentialHelper
				}
			}
			for _, k := range keys {
				e, ok := af.Auths[k]
				if !ok {
					continue
				}
				if e.IdentityToken != "" {
					return "", "", ErrCredentialHelper
				}
				if e.Username != "" {
					return e.Username, e.Password, nil
				}
				if e.Auth != "" {
					dec, err := base64.StdEncoding.DecodeString(e.Auth)
					if err != nil {
						return "", "", fmt.Errorf("read %s: invalid auth for %s", f, k)
					}
					user, pass, _ := strings.Cut(string(dec), ":")
					return user, pass, nil
				}
				if af.CredsStore != "" {
					return "", "", ErrCredentialHelper
				}
			}
		}
		return "", "", nil
	}
}

// hostKeys are the spellings auth files use for a registry.
func hostKeys(registry string) []string {
	if registry == DockerHub {
		return []string{"https://index.docker.io/v1/", "index.docker.io", "docker.io", "registry-1.docker.io", "https://registry-1.docker.io"}
	}
	return []string{registry, "https://" + registry, "http://" + registry, "https://" + registry + "/v1/", "https://" + registry + "/v2/"}
}
