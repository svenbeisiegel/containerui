package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/svenbeisiegel/containerui/internal/kube"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// Kubernetes mode: the pages show a cluster from kube.Cluster's in-memory
// copy instead of a container runtime. Everything is read-only for now.

// clusterRuntime stands in for the container runtime in Kubernetes mode,
// where no runtime routes are registered: only its name, empty capabilities
// and Info are ever asked for.
type clusterRuntime struct{ runtime.Runtime }

func (clusterRuntime) Name() string                       { return "kubernetes" }
func (clusterRuntime) Capabilities() runtime.Capabilities { return runtime.Capabilities{} }
func (clusterRuntime) Info(context.Context) (runtime.Info, error) {
	return runtime.Info{Driver: "kubernetes"}, nil
}

// nsCookie remembers the namespace chosen on the list pages.
const nsCookie = "cui_ns"

func (s *Server) registerKubeRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /{$}", s.handleKubeDashboard)
	m.HandleFunc("GET /partials/dashboard", s.handleKubeDashboardPartial)
	m.HandleFunc("GET /workloads", s.handleKubeList("workloads"))
	m.HandleFunc("GET /partials/workloads", s.handleKubeListPartial("workloads"))
	m.HandleFunc("GET /workloads/{ns}/{kind}/{name}", s.handleKubeWorkload)
	m.HandleFunc("GET /pods", s.handleKubeList("pods"))
	m.HandleFunc("GET /partials/pods", s.handleKubeListPartial("pods"))
	m.HandleFunc("GET /pods/{ns}/{name}", s.handleKubePod)
	m.HandleFunc("GET /services", s.handleKubeList("services"))
	m.HandleFunc("GET /partials/services", s.handleKubeListPartial("services"))
	m.HandleFunc("GET /services/{ns}/{name}", s.handleKubeService)
	m.HandleFunc("GET /storage", s.handleKubeList("storage"))
	m.HandleFunc("GET /partials/storage", s.handleKubeListPartial("storage"))
	m.HandleFunc("GET /storage/pvc/{ns}/{name}", s.handleKubePVC)
	m.HandleFunc("GET /nodes", s.handleKubeList("nodes"))
	m.HandleFunc("GET /partials/nodes", s.handleKubeListPartial("nodes"))
	m.HandleFunc("GET /nodes/{name}", s.handleKubeNode)
	m.HandleFunc("GET /cluster-events", s.handleKubeList("cluster_events"))
	m.HandleFunc("GET /partials/cluster-events", s.handleKubeListPartial("cluster_events"))
}

// ---- shared pieces ----

// nsFilter is the namespace picker of a list page.
type nsFilter struct {
	Selected   string   // "" = all
	Namespaces []string // offered
	Path       string   // the page, the picker's form action
	Query      url.Values
}

// Q returns the page's query with the namespace set, for links and
// partial refreshes.
func (f nsFilter) Q() string {
	q := url.Values{}
	for k, v := range f.Query {
		q[k] = v
	}
	if f.Selected != "" {
		q.Set("ns", f.Selected)
	} else {
		q.Del("ns")
	}
	return q.Encode()
}

// namespace picks the namespace of a list page: ?ns= (remembered in a
// cookie; "all" clears it), else the cookie. Unknown namespaces fall back
// to all.
func (s *Server) namespace(w http.ResponseWriter, r *http.Request, path string) nsFilter {
	f := nsFilter{Namespaces: s.kube.Namespaces(), Path: path, Query: url.Values{}}
	for k, v := range r.URL.Query() {
		if k != "ns" {
			f.Query[k] = v
		}
	}
	ns, set := r.URL.Query()["ns"]
	if set {
		f.Selected = ns[0]
		if f.Selected == "all" {
			f.Selected = ""
		}
		if w != nil && !isHTMX(r) {
			http.SetCookie(w, &http.Cookie{Name: nsCookie, Value: f.Selected, Path: "/", HttpOnly: true,
				Secure: s.secure, SameSite: http.SameSiteStrictMode, MaxAge: 30 * 24 * 3600})
		}
	} else if c, err := r.Cookie(nsCookie); err == nil {
		f.Selected = c.Value
	}
	if f.Selected != "" && !slices.Contains(f.Namespaces, f.Selected) {
		f.Selected = ""
	}
	return f
}

// kubeState is shown above a list: still loading, or a watch failing.
type kubeState struct {
	Loading bool
	Err     string
}

func (s *Server) kubeState(res ...kube.Resource) kubeState {
	st := s.kube.State(res...)
	return kubeState{Loading: !st.Synced && st.Err == "", Err: st.Err}
}

// kubeEvent is one row of an events table.
type kubeEvent struct {
	kube.Event
	Link string // the involved object's page, "" if none
}

// eventsFor returns the events of one object (kind "" = any), newest first.
func (s *Server) eventsFor(ns, kind, name string, limit int) []kubeEvent {
	var out []kubeEvent
	for _, e := range kube.List[kube.Event](s.kube, kube.ResEvents, ns) {
		o := e.InvolvedObject
		if (kind != "" && o.Kind != kind) || (name != "" && o.Name != name) {
			continue
		}
		out = append(out, kubeEvent{Event: e, Link: s.existingLink(o.Kind, o.Namespace, o.Name)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastSeen().After(out[j].LastSeen()) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// existingLink is kubeLink for objects still in the cluster: events outlive
// what they are about (a CronJob's finished jobs are deleted).
func (s *Server) existingLink(kind, ns, name string) string {
	res, ok := map[string]kube.Resource{
		"Pod": kube.ResPods, kube.KindDeployment: kube.ResDeployments, kube.KindStatefulSet: kube.ResStatefulSets,
		kube.KindDaemonSet: kube.ResDaemonSets, kube.KindJob: kube.ResJobs, kube.KindCronJob: kube.ResCronJobs,
		"Node": kube.ResNodes, "Service": kube.ResServices, "PersistentVolumeClaim": kube.ResPVCs,
	}[kind]
	if !ok {
		return ""
	}
	if !res.Namespaced {
		ns = ""
	}
	if _, found := s.kube.GetRaw(res, ns, name); !found {
		return ""
	}
	return kubeLink(kind, ns, name)
}

// kubeLink is the UI page of an object, "" if it has none.
func kubeLink(kind, ns, name string) string {
	esc := url.PathEscape
	switch kind {
	case "Pod":
		return "/pods/" + esc(ns) + "/" + esc(name)
	case kube.KindDeployment, kube.KindStatefulSet, kube.KindDaemonSet, kube.KindJob, kube.KindCronJob:
		return "/workloads/" + esc(ns) + "/" + kind + "/" + esc(name)
	case "Node":
		return "/nodes/" + esc(name)
	case "Service":
		return "/services/" + esc(ns) + "/" + esc(name)
	case "PersistentVolumeClaim":
		return "/storage/pvc/" + esc(ns) + "/" + esc(name)
	}
	return ""
}

// toYAML renders an object as YAML in its original key order, without the
// noise kubectl get -o yaml also hides (managed fields, the last applied
// configuration).
func toYAML(raw json.RawMessage) string {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return string(raw)
	}
	var clean func(n *yaml.Node)
	clean = func(n *yaml.Node) {
		n.Style = 0
		if n.Kind == yaml.MappingNode {
			kept := n.Content[:0]
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i].Value
				if k == "managedFields" || k == "kubectl.kubernetes.io/last-applied-configuration" {
					continue
				}
				kept = append(kept, n.Content[i], n.Content[i+1])
			}
			n.Content = kept
		}
		for _, c := range n.Content {
			clean(c)
		}
	}
	clean(&doc)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// podRow is a pod in a table.
type podRow struct {
	kube.Pod
	OwnerKind, OwnerName, OwnerLink string
}

func (s *Server) podRows(pods []kube.Pod) []podRow {
	rows := make([]podRow, 0, len(pods))
	for _, p := range pods {
		kind, name := s.kube.Owner(p.Metadata)
		rows = append(rows, podRow{Pod: p, OwnerKind: kind, OwnerName: name, OwnerLink: kubeLink(kind, p.Metadata.Namespace, name)})
	}
	return rows
}

// workloadPods returns the pods the workload controls: directly (a Job's
// pods) or as their top-level owner (a Deployment's pods through its
// ReplicaSets, a CronJob's through its Jobs).
func (s *Server) workloadPods(w kube.Workload) []kube.Pod {
	var out []kube.Pod
	for _, p := range kube.List[kube.Pod](s.kube, kube.ResPods, w.Meta.Namespace) {
		if ref := p.Metadata.Controller(); ref != nil && ref.Kind == w.Kind && ref.Name == w.Meta.Name {
			out = append(out, p)
		} else if kind, name := s.kube.Owner(p.Metadata); kind == w.Kind && name == w.Meta.Name {
			out = append(out, p)
		}
	}
	return out
}

// recent reports whether t lies within d before now.
func recent(t time.Time, d time.Duration) bool { return !t.IsZero() && time.Since(t) < d }

// ---- dashboard ----

type kubeDashboard struct {
	State          kubeState
	NodesReady     int
	Nodes          []kube.Node
	NodePods       map[string]int
	Pods           int
	PodsRunning    int
	PodsPending    int
	PodsFailing    int
	Workloads      int
	WorkloadsReady int
	Warnings       int // warning events in the last hour
	Unhealthy      []kube.Workload
	ProblemPods    []podRow
	RecentWarnings []kubeEvent
	Namespaces     int
	Limited        bool
}

func (s *Server) kubeDashboard() kubeDashboard {
	d := kubeDashboard{State: s.kubeState(kube.Watched...), NodePods: map[string]int{}, Limited: s.kube.Limited() != nil}
	d.Namespaces = len(s.kube.Namespaces())
	d.Nodes = kube.List[kube.Node](s.kube, kube.ResNodes, "")
	for _, n := range d.Nodes {
		if n.Ready() {
			d.NodesReady++
		}
	}
	var problems []kube.Pod
	for _, p := range kube.List[kube.Pod](s.kube, kube.ResPods, "") {
		d.NodePods[p.Spec.NodeName]++
		if p.Finished() {
			if p.Status.Phase == "Failed" {
				problems = append(problems, p)
			}
			continue
		}
		d.Pods++
		switch cls := p.StatusClass(); {
		case cls == "ok":
			d.PodsRunning++
		case p.Status.Phase == "Pending":
			d.PodsPending++
		case cls == "bad":
			d.PodsFailing++
		}
		if p.StatusClass() == "bad" || recent(p.LastRestart(), time.Hour) {
			problems = append(problems, p)
		}
	}
	d.ProblemPods = s.podRows(problems)
	for _, w := range s.kube.Workloads("") {
		if w.Kind == kube.KindJob && w.Meta.Controller() != nil {
			continue // a CronJob's run
		}
		d.Workloads++
		if w.Healthy() {
			d.WorkloadsReady++
		} else {
			d.Unhealthy = append(d.Unhealthy, w)
		}
	}
	for _, e := range s.eventsFor("", "", "", 0) {
		if !e.Warning() || !recent(e.LastSeen(), time.Hour) {
			continue
		}
		d.Warnings++
		if len(d.RecentWarnings) < 8 {
			d.RecentWarnings = append(d.RecentWarnings, e)
		}
	}
	return d
}

func (s *Server) handleKubeDashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, "kube_dashboard", s.newPage(r, "Dashboard", "dashboard", s.kubeDashboard()))
}

func (s *Server) handleKubeDashboardPartial(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, "kube_dashboard_body", s.kubeDashboard())
}

// ---- list pages ----

type kubeListPage struct {
	NS    nsFilter
	State kubeState
	Data  any
}

type workloadsList struct {
	Workloads []kube.Workload
	Runs      map[string]int // CronJob key → jobs kept
}

type podsList struct {
	Pods     []podRow
	Finished int  // finished pods hidden
	ShowAll  bool // finished pods shown
}

type serviceRow struct {
	kube.Service
	ReadyPods, Pods int
}

type servicesList struct {
	Services  []serviceRow
	Ingresses []kube.Ingress
}

type pvcRow struct {
	kube.PVC
	UsedBy []string // pod names
}

type storageList struct {
	PVCs    []pvcRow
	PVs     []kube.PV
	Classes []kube.StorageClass
}

type nodeRow struct {
	kube.Node
	Pods int
}

type eventsList struct {
	Events   []kubeEvent
	Warnings bool // only warnings
	Total    int
}

// kubeLists maps a list page to its title, nav item, path, resources and
// loader.
var kubeLists = map[string]struct {
	title, active, path string
	namespaced          bool
	res                 []kube.Resource
	load                func(s *Server, r *http.Request, ns string) any
}{
	"workloads": {"Workloads", "workloads", "/workloads", true, kube.WorkloadResources, (*Server).loadWorkloads},
	"pods":      {"Pods", "pods", "/pods", true, []kube.Resource{kube.ResPods}, (*Server).loadPods},
	"services":  {"Services", "services", "/services", true, []kube.Resource{kube.ResServices, kube.ResIngresses}, (*Server).loadServices},
	"storage":   {"Storage", "storage", "/storage", true, []kube.Resource{kube.ResPVCs, kube.ResPVs, kube.ResStorageClasses}, (*Server).loadStorage},
	"nodes":     {"Nodes", "nodes", "/nodes", false, []kube.Resource{kube.ResNodes}, (*Server).loadNodes},
	"cluster_events": {"Cluster events", "cluster_events", "/cluster-events", true, []kube.Resource{kube.ResEvents},
		(*Server).loadClusterEvents},
}

func (s *Server) kubeList(w http.ResponseWriter, r *http.Request, name string) kubeListPage {
	l := kubeLists[name]
	p := kubeListPage{State: s.kubeState(l.res...)}
	if l.namespaced {
		p.NS = s.namespace(w, r, l.path)
	}
	p.Data = l.load(s, r, p.NS.Selected)
	return p
}

func (s *Server) handleKubeList(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := kubeLists[name]
		s.render(w, "kube_"+name, s.newPage(r, l.title, l.active, s.kubeList(w, r, name)))
	}
}

func (s *Server) handleKubeListPartial(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.renderPartial(w, "kube_"+name+"_body", s.kubeList(w, r, name))
	}
}

func (s *Server) loadWorkloads(_ *http.Request, ns string) any {
	v := workloadsList{Runs: map[string]int{}}
	for _, wl := range s.kube.Workloads(ns) {
		if ref := wl.Meta.Controller(); wl.Kind == kube.KindJob && ref != nil && ref.Kind == kube.KindCronJob {
			v.Runs[wl.Meta.Namespace+"/"+ref.Name]++
			continue
		}
		v.Workloads = append(v.Workloads, wl)
	}
	return v
}

func (s *Server) loadPods(r *http.Request, ns string) any {
	v := podsList{ShowAll: r.URL.Query().Get("finished") == "1"}
	var pods []kube.Pod
	for _, p := range kube.List[kube.Pod](s.kube, kube.ResPods, ns) {
		if p.Finished() && !v.ShowAll {
			v.Finished++
			continue
		}
		pods = append(pods, p)
	}
	v.Pods = s.podRows(pods)
	return v
}

func (s *Server) loadServices(_ *http.Request, ns string) any {
	pods := kube.List[kube.Pod](s.kube, kube.ResPods, ns)
	var v servicesList
	for _, svc := range kube.List[kube.Service](s.kube, kube.ResServices, ns) {
		row := serviceRow{Service: svc}
		for _, p := range pods {
			if p.Metadata.Namespace == svc.Metadata.Namespace && !p.Finished() && kube.MatchLabels(svc.Spec.Selector, p.Metadata.Labels) {
				row.Pods++
				if p.ReadyCount() == len(p.Spec.Containers) {
					row.ReadyPods++
				}
			}
		}
		v.Services = append(v.Services, row)
	}
	v.Ingresses = kube.List[kube.Ingress](s.kube, kube.ResIngresses, ns)
	return v
}

// claimUsers maps "namespace/claim" to the pods mounting it.
func (s *Server) claimUsers(ns string) map[string][]string {
	users := map[string][]string{}
	for _, p := range kube.List[kube.Pod](s.kube, kube.ResPods, ns) {
		if p.Finished() {
			continue
		}
		for _, c := range p.Claims() {
			k := p.Metadata.Namespace + "/" + c
			users[k] = append(users[k], p.Metadata.Name)
		}
	}
	return users
}

func (s *Server) loadStorage(_ *http.Request, ns string) any {
	users := s.claimUsers(ns)
	var v storageList
	for _, c := range kube.List[kube.PVC](s.kube, kube.ResPVCs, ns) {
		v.PVCs = append(v.PVCs, pvcRow{PVC: c, UsedBy: users[c.Metadata.Key()]})
	}
	for _, pv := range kube.List[kube.PV](s.kube, kube.ResPVs, "") {
		if ns != "" && (pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Namespace != ns) {
			continue
		}
		if pv.Spec.ClaimRef != nil && s.kube.Limited() != nil && !slices.Contains(s.kube.Limited(), pv.Spec.ClaimRef.Namespace) {
			continue
		}
		v.PVs = append(v.PVs, pv)
	}
	v.Classes = kube.List[kube.StorageClass](s.kube, kube.ResStorageClasses, "")
	return v
}

func (s *Server) loadNodes(_ *http.Request, _ string) any {
	count := map[string]int{}
	for _, p := range kube.List[kube.Pod](s.kube, kube.ResPods, "") {
		if !p.Finished() {
			count[p.Spec.NodeName]++
		}
	}
	var rows []nodeRow
	for _, n := range kube.List[kube.Node](s.kube, kube.ResNodes, "") {
		rows = append(rows, nodeRow{Node: n, Pods: count[n.Metadata.Name]})
	}
	return rows
}

// maxEvents bounds the events table.
const maxEvents = 500

func (s *Server) loadClusterEvents(r *http.Request, ns string) any {
	v := eventsList{Warnings: r.URL.Query().Get("type") == "warning"}
	for _, e := range s.eventsFor(ns, "", "", 0) {
		if v.Warnings && !e.Warning() {
			continue
		}
		v.Total++
		if len(v.Events) < maxEvents {
			v.Events = append(v.Events, e)
		}
	}
	return v
}

// ---- detail pages ----

type kubeDetail struct {
	Kind, Namespace, Name string
	Back                  string
	Missing               bool // not (or no longer) in the cluster
	State                 kubeState
	Events                []kubeEvent
	YAML                  string
}

func (s *Server) detail(kind, ns, name, back string, raw json.RawMessage, found bool, res ...kube.Resource) kubeDetail {
	d := kubeDetail{Kind: kind, Namespace: ns, Name: name, Back: back, Missing: !found, State: s.kubeState(res...)}
	if found {
		d.YAML = toYAML(raw)
		d.Events = s.eventsFor(ns, kind, name, 50)
	}
	return d
}

func (s *Server) renderDetail(w http.ResponseWriter, r *http.Request, page string, title, active string, d kubeDetail, data any) {
	if d.Missing && !d.State.Loading {
		w.WriteHeader(http.StatusNotFound)
	}
	s.render(w, page, s.newPage(r, title, active, map[string]any{"D": d, "V": data}))
}

type podDetail struct {
	Pod  podRow
	Node string
}

func (s *Server) handleKubePod(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	p, raw, ok := kube.Get[kube.Pod](s.kube, kube.ResPods, ns, name)
	d := s.detail("Pod", ns, name, "/pods", raw, ok, kube.ResPods)
	var v podDetail
	if ok {
		v.Pod = s.podRows([]kube.Pod{*p})[0]
	}
	s.renderDetail(w, r, "kube_pod", name, "pods", d, v)
}

type workloadDetail struct {
	W    kube.Workload
	Pods []podRow
	Jobs []kube.Workload // a CronJob's runs, newest first
}

func (s *Server) handleKubeWorkload(w http.ResponseWriter, r *http.Request) {
	ns, kind, name := r.PathValue("ns"), r.PathValue("kind"), r.PathValue("name")
	wl, raw, ok := s.kube.Workload(kind, ns, name)
	d := s.detail(kind, ns, name, "/workloads", raw, ok, kube.WorkloadResources...)
	var v workloadDetail
	if ok {
		v.W = wl
		v.Pods = s.podRows(s.workloadPods(wl))
		if kind == kube.KindCronJob {
			for _, j := range s.kube.Workloads(ns) {
				if ref := j.Meta.Controller(); j.Kind == kube.KindJob && ref != nil && ref.Kind == kube.KindCronJob && ref.Name == name {
					v.Jobs = append(v.Jobs, j)
				}
			}
			sort.SliceStable(v.Jobs, func(i, j int) bool {
				return v.Jobs[i].Meta.CreationTimestamp.After(v.Jobs[j].Meta.CreationTimestamp)
			})
		}
	}
	s.renderDetail(w, r, "kube_workload", name, "workloads", d, v)
}

type serviceDetail struct {
	Svc  kube.Service
	Pods []podRow
}

func (s *Server) handleKubeService(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	svc, raw, ok := kube.Get[kube.Service](s.kube, kube.ResServices, ns, name)
	d := s.detail("Service", ns, name, "/services", raw, ok, kube.ResServices)
	var v serviceDetail
	if ok {
		v.Svc = *svc
		var pods []kube.Pod
		for _, p := range kube.List[kube.Pod](s.kube, kube.ResPods, ns) {
			if !p.Finished() && kube.MatchLabels(svc.Spec.Selector, p.Metadata.Labels) {
				pods = append(pods, p)
			}
		}
		v.Pods = s.podRows(pods)
	}
	s.renderDetail(w, r, "kube_service", name, "services", d, v)
}

type pvcDetail struct {
	PVC  kube.PVC
	PV   *kube.PV
	Pods []podRow
}

func (s *Server) handleKubePVC(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	c, raw, ok := kube.Get[kube.PVC](s.kube, kube.ResPVCs, ns, name)
	d := s.detail("PersistentVolumeClaim", ns, name, "/storage", raw, ok, kube.ResPVCs)
	var v pvcDetail
	if ok {
		v.PVC = *c
		if c.Spec.VolumeName != "" {
			v.PV, _, _ = kube.Get[kube.PV](s.kube, kube.ResPVs, "", c.Spec.VolumeName)
		}
		var pods []kube.Pod
		for _, p := range kube.List[kube.Pod](s.kube, kube.ResPods, ns) {
			if !p.Finished() && slices.Contains(p.Claims(), name) {
				pods = append(pods, p)
			}
		}
		v.Pods = s.podRows(pods)
	}
	s.renderDetail(w, r, "kube_pvc", name, "storage", d, v)
}

type nodeResource struct {
	Name, Capacity, Allocatable string
}

type nodeDetail struct {
	Node      kube.Node
	Resources []nodeResource
	Pods      []podRow
	Images    []nodeImageRow
	ImageSize int64 // of all images listed
	// ImagesCapped: the kubelet listed as many images as it reports at
	// most, so the node may store more.
	ImagesCapped bool
}

// nodeImageRow is an image stored on a node, with the pods there using it.
type nodeImageRow struct {
	kube.NodeImage
	UsedBy []podRef
}

type podRef struct{ Namespace, Name string }

// nodeImages lists the node's images, the ones running pods use first,
// then by repository.
func nodeImages(n kube.Node, pods []kube.Pod) []nodeImageRow {
	rows := make([]nodeImageRow, 0, len(n.Status.Images))
	for _, img := range n.Status.Images {
		row := nodeImageRow{NodeImage: img}
		for _, p := range pods {
			for _, cs := range append(append([]kube.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
				if img.Has(cs.ImageID) || img.Has(cs.Image) {
					row.UsedBy = append(row.UsedBy, podRef{p.Metadata.Namespace, p.Metadata.Name})
					break
				}
			}
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if a, b := len(rows[i].UsedBy) > 0, len(rows[j].UsedBy) > 0; a != b {
			return a
		}
		return rows[i].Repo() < rows[j].Repo()
	})
	return rows
}

func (s *Server) handleKubeNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	n, raw, ok := kube.Get[kube.Node](s.kube, kube.ResNodes, "", name)
	d := s.detail("Node", "", name, "/nodes", raw, ok, kube.ResNodes)
	var v nodeDetail
	if ok {
		v.Node = *n
		var keys []string
		for k := range n.Status.Capacity {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v.Resources = append(v.Resources, nodeResource{k, quantity(k, n.Status.Capacity[k]), quantity(k, n.Status.Allocatable[k])})
		}
		var pods []kube.Pod
		for _, p := range kube.List[kube.Pod](s.kube, kube.ResPods, "") {
			if p.Spec.NodeName == name && !p.Finished() {
				pods = append(pods, p)
			}
		}
		v.Pods = s.podRows(pods)
		v.Images = nodeImages(*n, pods)
		for _, img := range n.Status.Images {
			v.ImageSize += img.SizeBytes
		}
		v.ImagesCapped = len(n.Status.Images) >= kube.MaxNodeImages
	}
	s.renderDetail(w, r, "kube_node", name, "nodes", d, v)
}

// quantity formats a resource quantity for display: memory and storage as
// bytes, the rest as given.
func quantity(resource, q string) string {
	if q == "" {
		return "–"
	}
	if resource == "memory" || strings.Contains(resource, "storage") || strings.HasPrefix(resource, "hugepages-") {
		if b := kube.Bytes(q); b > 0 {
			return humanBytes(b)
		}
	}
	return q
}
