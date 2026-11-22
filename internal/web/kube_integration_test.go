//go:build kubeintegration

// Renders every Kubernetes page against a real cluster (read-only):
//
//	go test -tags kubeintegration ./internal/web/ -run KubeIntegration -v
//
// The kubeconfig is found like kube.LoadConfig does.
package web

import (
	"context"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/config"
	"github.com/svenbeisiegel/containerui/internal/kube"
)

func TestKubeIntegrationPages(t *testing.T) {
	kc, err := kube.LoadConfig("", "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := kube.New(kc, kube.Options{UserAgent: "containerui-integration-test"})
	if err != nil {
		t.Fatal(err)
	}
	// The namespace list as the shipped default config sets it.
	cfg, err := config.Parse(config.DefaultYAML)
	if err != nil {
		t.Fatal(err)
	}
	cl := kube.NewCluster(c, cfg.Kubernetes.Namespaces, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cl.Run(ctx)
	syncCtx, cancelSync := context.WithTimeout(ctx, 30*time.Second)
	defer cancelSync()
	start := time.Now()
	if !cl.WaitSynced(syncCtx) {
		t.Fatalf("not synced: %+v", cl.State(kube.Watched...))
	}
	t.Logf("synced %d resources in %s", len(kube.Watched), time.Since(start).Round(time.Millisecond))

	e := newEnvWith(t, func(o *Options) { o.Runtime, o.Kube = nil, cl })
	e.login()

	pages := []string{"/", "/workloads", "/pods", "/pods?finished=1", "/services", "/storage", "/nodes", "/cluster-events", "/cluster-events?type=warning", "/pods?ns=osem"}
	link := regexp.MustCompile(`href="(/(?:pods|workloads|services|storage/pvc|nodes)/[^"?]+)"`)
	seen := map[string]bool{}
	for i := 0; i < len(pages); i++ {
		p := pages[i]
		start := time.Now()
		w := e.do("GET", p, nil)
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d", p, w.Code)
			continue
		}
		t.Logf("%-70s %6d bytes %s", p, w.Body.Len(), time.Since(start).Round(time.Microsecond))
		for _, m := range link.FindAllStringSubmatch(w.Body.String(), -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				pages = append(pages, m[1])
			}
		}
	}
	t.Logf("rendered %d pages", len(pages))
	if len(pages) < 20 {
		t.Errorf("only %d pages: the cluster's namespaced objects are missing", len(pages))
	}
}
