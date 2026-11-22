// Package kube talks to the Kubernetes API server over plain HTTPS, like
// package dockerapi talks to the Docker Engine API: no client library, only
// the endpoints ContainerUI needs. This file loads the connection settings
// from a kubeconfig file or, inside a pod, from the service account.
package kube

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds what is needed to reach and authenticate to an API server.
type Config struct {
	Server     string // https://host:port, optionally with a path prefix
	Namespace  string // the context's namespace, "default" if unset
	CAData     []byte // PEM; empty = the system's roots
	CertData   []byte // PEM client certificate
	KeyData    []byte // PEM client key
	Token      string // bearer token
	TokenFile  string // bearer token re-read from this file (rotated tokens)
	Insecure   bool   // skip verifying the server's certificate
	ServerName string // TLS server name, if it differs from the host

	Source  string // the kubeconfig file, or InCluster
	Context string // the kubeconfig context used
}

// InCluster is Config.Source for the pod's service account.
const InCluster = "in-cluster"

// serviceAccountDir is where Kubernetes mounts a pod's service account
// (a variable for tests).
var serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// DefaultPaths lists the kubeconfig files LoadConfig tries without an
// explicit path, in order: $KUBECONFIG (each entry), ~/.kube/config, then
// the admin kubeconfigs RKE2 and K3s write on their server nodes.
func DefaultPaths() []string {
	var paths []string
	for _, p := range filepath.SplitList(os.Getenv("KUBECONFIG")) {
		if p != "" {
			paths = append(paths, p)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".kube", "config"))
	}
	return append(paths, "/etc/rancher/rke2/rke2.yaml", "/etc/rancher/k3s/k3s.yaml")
}

// LoadConfig reads the kubeconfig at path, using its context named
// kubeContext (empty: its current context). With an empty path it uses the
// first of DefaultPaths that exists and, if none does, the pod's service
// account.
func LoadConfig(path, kubeContext string) (*Config, error) {
	if path != "" {
		return loadFile(path, kubeContext)
	}
	tried := DefaultPaths()
	for _, p := range tried {
		if _, err := os.Stat(p); err == nil {
			return loadFile(p, kubeContext)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("kubeconfig %s: %w", p, err)
		}
	}
	if cfg, err := inClusterConfig(); err == nil {
		return cfg, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return nil, fmt.Errorf("no kubeconfig found (tried %s) and not running in a pod; set kubernetes.kubeconfig", strings.Join(tried, ", "))
}

// kubeconfig is the part of the kubeconfig format ContainerUI reads.
type kubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server     string `yaml:"server"`
			CA         string `yaml:"certificate-authority"`
			CAData     string `yaml:"certificate-authority-data"`
			Insecure   bool   `yaml:"insecure-skip-tls-verify"`
			ServerName string `yaml:"tls-server-name"`
			ProxyURL   string `yaml:"proxy-url"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Cert         string `yaml:"client-certificate"`
			CertData     string `yaml:"client-certificate-data"`
			Key          string `yaml:"client-key"`
			KeyData      string `yaml:"client-key-data"`
			Token        string `yaml:"token"`
			TokenFile    string `yaml:"tokenFile"`
			Username     string `yaml:"username"`
			Exec         any    `yaml:"exec"`
			AuthProvider any    `yaml:"auth-provider"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster   string `yaml:"cluster"`
			User      string `yaml:"user"`
			Namespace string `yaml:"namespace"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

func loadFile(path, kubeContext string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	cfg, err := parseKubeconfig(data, filepath.Dir(path), kubeContext)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", path, err)
	}
	cfg.Source = path
	return cfg, nil
}

// parseKubeconfig builds a Config from a kubeconfig document; relative
// file references are resolved against dir.
func parseKubeconfig(data []byte, dir, kubeContext string) (*Config, error) {
	var kc kubeconfig
	if err := yaml.Unmarshal(data, &kc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	name := kubeContext
	if name == "" {
		name = kc.CurrentContext
	}
	if name == "" {
		if len(kc.Contexts) != 1 {
			return nil, errors.New("no current-context set; set kubernetes.context")
		}
		name = kc.Contexts[0].Name
	}
	ci := -1
	for i, c := range kc.Contexts {
		if c.Name == name {
			ci = i
			break
		}
	}
	if ci < 0 {
		return nil, fmt.Errorf("context %q not found", name)
	}
	ctx := kc.Contexts[ci].Context

	cfg := &Config{Context: name, Namespace: ctx.Namespace}
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	file := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}

	found := false
	for _, c := range kc.Clusters {
		if c.Name != ctx.Cluster {
			continue
		}
		found = true
		cl := c.Cluster
		if cl.ProxyURL != "" {
			return nil, fmt.Errorf("cluster %q: proxy-url is not supported", c.Name)
		}
		cfg.Server = strings.TrimRight(cl.Server, "/")
		cfg.Insecure = cl.Insecure
		cfg.ServerName = cl.ServerName
		var err error
		if cfg.CAData, err = dataOrFile(cl.CAData, file(cl.CA)); err != nil {
			return nil, fmt.Errorf("cluster %q: certificate authority: %w", c.Name, err)
		}
		break
	}
	if !found {
		return nil, fmt.Errorf("context %q: cluster %q not found", name, ctx.Cluster)
	}
	if !strings.HasPrefix(cfg.Server, "https://") && !strings.HasPrefix(cfg.Server, "http://") {
		return nil, fmt.Errorf("cluster %q: server %q is not an http(s) URL", ctx.Cluster, cfg.Server)
	}

	found = false
	for _, u := range kc.Users {
		if u.Name != ctx.User {
			continue
		}
		found = true
		us := u.User
		switch {
		case us.Exec != nil:
			return nil, fmt.Errorf("user %q: exec credential plugins are not supported; use a kubeconfig with a client certificate or token", u.Name)
		case us.AuthProvider != nil:
			return nil, fmt.Errorf("user %q: auth-provider is not supported; use a kubeconfig with a client certificate or token", u.Name)
		case us.Username != "":
			return nil, fmt.Errorf("user %q: basic authentication is not supported; use a client certificate or token", u.Name)
		}
		var err error
		if cfg.CertData, err = dataOrFile(us.CertData, file(us.Cert)); err != nil {
			return nil, fmt.Errorf("user %q: client certificate: %w", u.Name, err)
		}
		if cfg.KeyData, err = dataOrFile(us.KeyData, file(us.Key)); err != nil {
			return nil, fmt.Errorf("user %q: client key: %w", u.Name, err)
		}
		if (len(cfg.CertData) == 0) != (len(cfg.KeyData) == 0) {
			return nil, fmt.Errorf("user %q: client certificate and key must both be set", u.Name)
		}
		cfg.Token = us.Token
		if cfg.Token == "" {
			cfg.TokenFile = file(us.TokenFile)
		}
		break
	}
	// A context without a user is allowed (anonymous access).
	if !found && ctx.User != "" {
		return nil, fmt.Errorf("context %q: user %q not found", name, ctx.User)
	}
	return cfg, nil
}

// dataOrFile returns the base64-decoded inline data, else the file's content.
func dataOrFile(b64, path string) ([]byte, error) {
	if b64 != "" {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return nil, fmt.Errorf("invalid base64: %w", err)
		}
		return b, nil
	}
	if path == "" {
		return nil, nil
	}
	return os.ReadFile(path)
}

// inClusterConfig uses the service account Kubernetes mounts into a pod.
// It returns an error wrapping fs.ErrNotExist when not running in a pod.
func inClusterConfig() (*Config, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not in a pod: %w", fs.ErrNotExist)
	}
	tokenFile := filepath.Join(serviceAccountDir, "token")
	if _, err := os.Stat(tokenFile); err != nil {
		return nil, fmt.Errorf("service account token: %w", err)
	}
	ca, err := os.ReadFile(filepath.Join(serviceAccountDir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("service account CA: %w", err)
	}
	ns := "default"
	if b, err := os.ReadFile(filepath.Join(serviceAccountDir, "namespace")); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		ns = strings.TrimSpace(string(b))
	}
	return &Config{
		Server:    "https://" + net.JoinHostPort(host, port),
		Namespace: ns,
		CAData:    ca,
		TokenFile: tokenFile,
		Source:    InCluster,
	}, nil
}
