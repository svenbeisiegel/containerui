package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAPI serves lists and watches of pods: the first watch adds wpi-2 and
// deletes wpi-0, the second fails with 410 Gone, which makes the cache list
// again; later watches block until the request ends.
type fakeAPI struct {
	mu      sync.Mutex
	lists   int
	watches atomic.Int32
	paths   []string
	check   bool // assert the resource versions watches start from
}

func (f *fakeAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/pods") {
			fmt.Fprint(w, `{"metadata":{"resourceVersion":"1"},"items":[]}`)
			if q.Get("watch") == "1" {
				<-r.Context().Done()
			}
			return
		}
		pod := func(name, rv string) string {
			return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":"osem","resourceVersion":%q},"status":{"phase":"Running"}}`, name, rv)
		}
		if q.Get("watch") != "1" {
			f.mu.Lock()
			f.lists++
			n := f.lists
			f.mu.Unlock()
			if n == 1 {
				fmt.Fprintf(w, `{"metadata":{"resourceVersion":"10"},"items":[%s,%s]}`, pod("wpi-0", "9"), pod("wpi-1", "10"))
			} else {
				fmt.Fprintf(w, `{"metadata":{"resourceVersion":"30"},"items":[%s]}`, pod("wpi-9", "30"))
			}
			return
		}
		fl := w.(http.Flusher)
		switch f.watches.Add(1) {
		case 1:
			if f.check && q.Get("resourceVersion") != "10" {
				t.Errorf("first watch from %q, want 10", q.Get("resourceVersion"))
			}
			fmt.Fprintln(w, `{"type":"ADDED","object":`+pod("wpi-2", "11")+`}`)
			fmt.Fprintln(w, `{"type":"DELETED","object":`+pod("wpi-0", "12")+`}`)
			fmt.Fprintln(w, `{"type":"BOOKMARK","object":{"metadata":{"resourceVersion":"20"}}}`)
		case 2:
			if f.check && q.Get("resourceVersion") != "20" {
				t.Errorf("second watch from %q, want the bookmark's 20", q.Get("resourceVersion"))
			}
			fmt.Fprintln(w, `{"type":"ERROR","object":{"kind":"Status","code":410,"reason":"Expired","message":"too old"}}`)
		default:
			fl.Flush()
			<-r.Context().Done()
		}
	}
}

func podNames(cl *Cluster) string {
	var names []string
	for _, p := range List[Pod](cl, ResPods, "") {
		names = append(names, p.Metadata.Name)
	}
	return strings.Join(names, ",")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClusterListWatchRelist(t *testing.T) {
	api := &fakeAPI{check: true}
	c := testClient(t, api.handler(t))
	cl := NewCluster(c, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cl.Run(ctx)
	if !cl.WaitSynced(ctx) {
		t.Fatalf("not synced: %+v", cl.State(Watched...))
	}
	// After the first watch: wpi-0 deleted, wpi-2 added; after the 410 the
	// list replaces everything with wpi-9.
	waitFor(t, "relist after 410", func() bool { return podNames(cl) == "wpi-9" })
	if api.watches.Load() < 3 {
		t.Errorf("watches = %d, want a third one after the relist", api.watches.Load())
	}
	if st := cl.State(ResPods); !st.Synced || st.Err != "" {
		t.Errorf("state = %+v", st)
	}
}

func TestClusterLimitedNamespaces(t *testing.T) {
	api := &fakeAPI{}
	c := testClient(t, api.handler(t))
	cl := NewCluster(c, []string{"osem", "monitoring"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cl.Run(ctx)
	cl.WaitSynced(ctx)
	api.mu.Lock()
	paths := strings.Join(api.paths, " ")
	api.mu.Unlock()
	for _, want := range []string{"/api/v1/namespaces/osem/pods", "/api/v1/namespaces/monitoring/pods", "/apis/apps/v1/namespaces/osem/statefulsets", "/api/v1/nodes"} {
		if !strings.Contains(paths, want) {
			t.Errorf("no request to %s", want)
		}
	}
	if strings.Contains(paths, "/api/v1/pods ") || strings.Contains(paths, "/api/v1/namespaces ") {
		t.Error("limited namespaces must not watch all pods or list namespaces")
	}
	if got := strings.Join(cl.Namespaces(), ","); got != "osem,monitoring" {
		t.Errorf("namespaces = %s", got)
	}
}

func TestClusterWatchErrorKeepsData(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/nodes") {
			fmt.Fprint(w, `{"metadata":{"resourceVersion":"1"},"items":[]}`)
			if r.URL.Query().Get("watch") == "1" {
				<-r.Context().Done()
			}
			return
		}
		if r.URL.Query().Get("watch") != "1" {
			fmt.Fprint(w, `{"metadata":{"resourceVersion":"5"},"items":[{"metadata":{"name":"kube-1"}}]}`)
			return
		}
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"kind":"Status","reason":"Forbidden","message":"nodes is forbidden: watch","code":403}`)
	})
	cl := NewCluster(c, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cl.Run(ctx)
	cl.WaitSynced(ctx)
	waitFor(t, "watch error", func() bool { return cl.State(ResNodes).Err != "" })
	if st := cl.State(ResNodes); !strings.Contains(st.Err, "nodes is forbidden") || !st.Synced {
		t.Errorf("state = %+v", st)
	}
	if n := len(List[Node](cl, ResNodes, "")); n != 1 {
		t.Errorf("listed nodes must stay while the watch fails: %d", n)
	}
}

func TestOwnerAndWorkloads(t *testing.T) {
	cl := NewStatic(nil)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	must(cl.Seed(ResDeployments, raw(`{"metadata":{"name":"web","namespace":"a"},"spec":{"replicas":2},"status":{"readyReplicas":1,"updatedReplicas":2}}`)))
	must(cl.Seed(ResReplicaSets, raw(`{"metadata":{"name":"web-5d","namespace":"a","ownerReferences":[{"kind":"Deployment","name":"web","controller":true}]}}`)))
	must(cl.Seed(ResCronJobs, raw(`{"metadata":{"name":"tick","namespace":"a"},"spec":{"schedule":"*/5 * * * *","suspend":true}}`)))
	must(cl.Seed(ResJobs, raw(`{"metadata":{"name":"tick-1","namespace":"a","ownerReferences":[{"kind":"CronJob","name":"tick","controller":true}]},"status":{"conditions":[{"type":"Failed","status":"True"}]}}`)))
	pod := Meta{Namespace: "a", OwnerReferences: []OwnerRef{{Kind: "ReplicaSet", Name: "web-5d", Controller: true}}}
	if k, n := cl.Owner(pod); k != KindDeployment || n != "web" {
		t.Errorf("owner = %s/%s", k, n)
	}
	job := Meta{Namespace: "a", OwnerReferences: []OwnerRef{{Kind: "Job", Name: "tick-1", Controller: true}}}
	if k, n := cl.Owner(job); k != KindCronJob || n != "tick" {
		t.Errorf("owner = %s/%s", k, n)
	}
	if k, _ := cl.Owner(Meta{}); k != "" {
		t.Error("a bare pod has no owner")
	}
	ws := cl.Workloads("a")
	if len(ws) != 3 || ws[0].Kind != KindDeployment || ws[1].Kind != KindCronJob || ws[2].Kind != KindJob {
		t.Fatalf("workloads = %+v", ws)
	}
	if ws[0].Healthy() || ws[0].ReadyText() != "1/2" {
		t.Errorf("deployment = %+v", ws[0])
	}
	if !ws[1].Suspended || ws[1].Extra[0].Value != "*/5 * * * *" {
		t.Errorf("cronjob = %+v", ws[1])
	}
	if !ws[2].Failed || ws[2].Healthy() {
		t.Errorf("job = %+v", ws[2])
	}
}

func TestPodStatusText(t *testing.T) {
	for want, doc := range map[string]string{
		"Running":          `{"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}}]}}`,
		"CrashLoopBackOff": `{"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}},{"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}`,
		"Completed":        `{"status":{"phase":"Succeeded","containerStatuses":[{"state":{"terminated":{"reason":"Completed"}}}]}}`,
		"Init:0/1":         `{"spec":{"initContainers":[{"name":"i"}]},"status":{"phase":"Pending","initContainerStatuses":[{"state":{"running":{}}}]}}`,
		"Init:Error":       `{"spec":{"initContainers":[{"name":"i"}]},"status":{"phase":"Pending","initContainerStatuses":[{"state":{"terminated":{"exitCode":1}}}]}}`,
		"Terminating":      `{"metadata":{"deletionTimestamp":"2026-11-22T20:00:00Z"},"status":{"phase":"Running"}}`,
		"Evicted":          `{"status":{"phase":"Failed","reason":"Evicted"}}`,
		"ExitCode:3":       `{"status":{"phase":"Running","containerStatuses":[{"state":{"terminated":{"exitCode":3}}}]}}`,
	} {
		var p Pod
		if err := json.Unmarshal([]byte(doc), &p); err != nil {
			t.Fatal(err)
		}
		if got := p.StatusText(); got != want {
			t.Errorf("%s: got %q", doc, got)
		}
	}
}

func TestParseQuantity(t *testing.T) {
	for q, want := range map[string]float64{"16379432Ki": 16379432 * 1024, "225Gi": 225 << 30, "500m": 0.5, "6": 6, "1.5G": 1.5e9, "559436396n": 0.559436396} {
		if got, ok := ParseQuantity(q); !ok || math.Abs(got-want) > want*1e-12 {
			t.Errorf("%s = %v %v, want %v", q, got, ok, want)
		}
	}
	for _, q := range []string{"", "abc", "1Zi"} {
		if _, ok := ParseQuantity(q); ok {
			t.Errorf("%q must not parse", q)
		}
	}
	if Bytes("1Mi") != 1<<20 || Cores("250m") != 0.25 {
		t.Error("Bytes/Cores")
	}
}

func TestResourcePath(t *testing.T) {
	for want, got := range map[string]string{
		"/api/v1/pods":                               ResPods.Path(""),
		"/api/v1/namespaces/osem/pods":               ResPods.Path("osem"),
		"/apis/apps/v1/namespaces/osem/statefulsets": ResStatefulSets.Path("osem"),
		"/api/v1/nodes":                              ResNodes.Path("osem"),
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

func TestEmptyNamespaceListMeansAll(t *testing.T) {
	cl := NewCluster(nil, []string{}, nil)
	if cl.Limited() != nil {
		t.Errorf("Limited() = %v, want nil for an empty list", cl.Limited())
	}
	for k := range cl.scopes {
		if k.ns != "" {
			t.Errorf("per-namespace scope %v for an empty list", k)
		}
	}
	if NewStatic([]string{}).Limited() != nil {
		t.Error("NewStatic: empty list must mean all")
	}
}
