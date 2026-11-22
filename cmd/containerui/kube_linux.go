package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/svenbeisiegel/containerui/internal/auth"
	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/kube"
)

// openKube connects to the cluster of the kubernetes config section and
// returns its (not yet started) cache and the kubeconfig file in use ("" for
// a pod's service account).
func openKube(cfg *config.Config, log *slog.Logger) (*kube.Cluster, string, error) {
	k := cfg.Kubernetes
	path := ""
	if k.Kubeconfig != "" {
		path = cfg.Resolve(k.Kubeconfig)
	}
	kc, err := kube.LoadConfig(path, k.Context)
	if err != nil {
		return nil, "", err
	}
	client, err := kube.New(kc, kube.Options{UserAgent: "containerui/" + version, Timeout: k.RequestTimeout})
	if err != nil {
		return nil, "", err
	}
	log.Info("kubernetes mode", "kubeconfig", kc.Source, "context", kc.Context, "server", kc.Server, "namespaces", k.Namespaces)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if v, err := client.Version(ctx); err != nil {
		log.Warn("kubernetes API server not reachable; the UI will show errors until it is", "server", kc.Server, "error", err)
	} else if u, err := client.WhoAmI(ctx); err != nil {
		log.Warn("kubernetes API server rejects the credentials or cannot tell who they are", "version", v.GitVersion, "error", err)
	} else {
		log.Info("kubernetes API server", "version", v.GitVersion, "user", u.Username, "groups", u.Groups)
	}

	file := kc.Source
	if file == kube.InCluster {
		file = ""
	}
	return kube.NewCluster(client, k.Namespaces, log), file, nil
}

// requireKubeconfig lets only accounts that may read the kubeconfig sign in.
func requireKubeconfig(sys *auth.System, path string) { sys.RequireReadable(path) }
