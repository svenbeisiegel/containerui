//go:build kubeintegration

// Read-only checks against a real cluster:
//
//	go test -tags kubeintegration ./internal/kube/ -run Integration -v
//
// The kubeconfig is found like LoadConfig does ($KUBECONFIG,
// ~/.kube/config, /etc/rancher/rke2/rke2.yaml, ...); KUBE_TEST_NAMESPACE
// picks the namespace whose pods are listed (default: the context's).
package kube

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestIntegration(t *testing.T) {
	cfg, err := LoadConfig("", "")
	if err != nil {
		t.Fatal(err)
	}
	ns := os.Getenv("KUBE_TEST_NAMESPACE")
	if ns == "" {
		ns = cfg.Namespace
	}
	t.Logf("kubeconfig %s, context %q, server %s, namespace %s", cfg.Source, cfg.Context, cfg.Server, ns)
	c, err := New(cfg, Options{UserAgent: "containerui-integration-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	v, err := c.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u, err := c.WhoAmI(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("server %s, authenticated as %s %v", v.GitVersion, u.Username, u.Groups)

	type pod struct {
		Metadata struct{ Name string } `json:"metadata"`
		Spec     struct {
			NodeName   string `json:"nodeName"`
			Containers []struct{ Name string }
		} `json:"spec"`
		Status struct{ Phase string } `json:"status"`
	}
	items, rv, err := c.List(ctx, Path("api", "v1", "namespaces", ns, "pods"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d pods at resourceVersion %s", len(items), rv)
	var running *pod
	for _, raw := range items {
		var p pod
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		t.Logf("  %-45s %-10s %-8s %d containers", p.Metadata.Name, p.Status.Phase, p.Spec.NodeName, len(p.Spec.Containers))
		if running == nil && p.Status.Phase == "Running" {
			running = &p
		}
	}

	// A short watch from the list's version ends by itself.
	events := 0
	q := url.Values{"resourceVersion": {rv}, "timeoutSeconds": {"3"}}
	if err := c.Watch(ctx, Path("api", "v1", "namespaces", ns, "pods"), q, func(ev WatchEvent) error {
		events++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("watch: %d events in 3 s", events)

	if running != nil {
		cn := running.Spec.Containers[0].Name
		rc, err := c.Stream(ctx, Path("api", "v1", "namespaces", ns, "pods", running.Metadata.Name, "log"),
			url.Values{"container": {cn}, "tailLines": {"3"}})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(io.LimitReader(rc, 4096))
		rc.Close()
		t.Logf("last log lines of %s/%s:\n%s", running.Metadata.Name, cn, b)
	}

	var nodeMetrics struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
			Usage    map[string]string     `json:"usage"`
		} `json:"items"`
	}
	if err := c.Get(ctx, "/apis/metrics.k8s.io/v1beta1/nodes", nil, &nodeMetrics); err != nil {
		t.Errorf("metrics-server: %v", err)
	}
	for _, n := range nodeMetrics.Items {
		t.Logf("node %s: cpu %s, memory %s", n.Metadata.Name, n.Usage["cpu"], n.Usage["memory"])
	}
	if len(nodeMetrics.Items) > 0 {
		var summary struct {
			Node struct {
				NodeName string `json:"nodeName"`
				Fs       struct {
					CapacityBytes, UsedBytes uint64
				} `json:"fs"`
			} `json:"node"`
			Pods []json.RawMessage `json:"pods"`
		}
		name := nodeMetrics.Items[0].Metadata.Name
		if err := c.Get(ctx, Path("api", "v1", "nodes", name, "proxy", "stats", "summary"), nil, &summary); err != nil {
			t.Errorf("node summary: %v", err)
		} else {
			t.Logf("node %s summary: fs %d/%d bytes used, %d pods", summary.Node.NodeName, summary.Node.Fs.UsedBytes, summary.Node.Fs.CapacityBytes, len(summary.Pods))
		}
	}
}
