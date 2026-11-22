package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Resource is a collection the Cluster watches.
type Resource struct {
	Name       string // plural, e.g. "pods"
	Group      string // "" for the core group
	Version    string
	Namespaced bool
}

// Path is the collection's URL path, within namespace if set.
func (r Resource) Path(namespace string) string {
	segs := []string{"api", r.Version}
	if r.Group != "" {
		segs = []string{"apis", r.Group, r.Version}
	}
	if r.Namespaced && namespace != "" {
		segs = append(segs, "namespaces", namespace)
	}
	return Path(append(segs, r.Name)...)
}

// The watched resources.
var (
	ResNamespaces     = Resource{Name: "namespaces", Version: "v1"}
	ResNodes          = Resource{Name: "nodes", Version: "v1"}
	ResPods           = Resource{Name: "pods", Version: "v1", Namespaced: true}
	ResServices       = Resource{Name: "services", Version: "v1", Namespaced: true}
	ResPVCs           = Resource{Name: "persistentvolumeclaims", Version: "v1", Namespaced: true}
	ResPVs            = Resource{Name: "persistentvolumes", Version: "v1"}
	ResEvents         = Resource{Name: "events", Version: "v1", Namespaced: true}
	ResDeployments    = Resource{Name: "deployments", Group: "apps", Version: "v1", Namespaced: true}
	ResStatefulSets   = Resource{Name: "statefulsets", Group: "apps", Version: "v1", Namespaced: true}
	ResDaemonSets     = Resource{Name: "daemonsets", Group: "apps", Version: "v1", Namespaced: true}
	ResReplicaSets    = Resource{Name: "replicasets", Group: "apps", Version: "v1", Namespaced: true}
	ResJobs           = Resource{Name: "jobs", Group: "batch", Version: "v1", Namespaced: true}
	ResCronJobs       = Resource{Name: "cronjobs", Group: "batch", Version: "v1", Namespaced: true}
	ResIngresses      = Resource{Name: "ingresses", Group: "networking.k8s.io", Version: "v1", Namespaced: true}
	ResStorageClasses = Resource{Name: "storageclasses", Group: "storage.k8s.io", Version: "v1"}
)

// Watched lists every resource a Cluster keeps.
var Watched = []Resource{
	ResNamespaces, ResNodes, ResPods, ResServices, ResPVCs, ResPVs, ResEvents,
	ResDeployments, ResStatefulSets, ResDaemonSets, ResReplicaSets, ResJobs, ResCronJobs,
	ResIngresses, ResStorageClasses,
}

// Cluster keeps an in-memory copy of the watched resources, updated by one
// list+watch loop per resource (per namespace when the namespaces are
// limited), so pages render from memory.
type Cluster struct {
	client     *Client
	namespaces []string // allowed namespaces; nil = all
	log        *slog.Logger

	mu     sync.RWMutex
	scopes map[scopeKey]*scope
}

type scopeKey struct {
	res string // Resource.Name
	ns  string // "" = all namespaces / cluster-scoped
}

type scope struct {
	objs   map[string]json.RawMessage // by Meta.Key()
	synced bool
	err    error
}

// NewCluster returns a cache over c. namespaces limits it to those
// namespaces (empty: all). Run starts it.
func NewCluster(c *Client, namespaces []string, log *slog.Logger) *Cluster {
	if log == nil {
		log = slog.Default()
	}
	cl := &Cluster{client: c, namespaces: limit(namespaces), log: log, scopes: map[scopeKey]*scope{}}
	for _, k := range cl.scopeKeys() {
		cl.scopes[k] = &scope{objs: map[string]json.RawMessage{}}
	}
	return cl
}

// NewStatic returns a cache without an API server whose content is set
// with Seed: for tests and demos.
func NewStatic(namespaces []string) *Cluster {
	return &Cluster{namespaces: limit(namespaces), log: slog.Default(), scopes: map[scopeKey]*scope{}}
}

// limit turns a namespace list into the allowed set: nil (all) for an
// empty list, which is what "namespaces: []" in the config means.
func limit(namespaces []string) []string {
	if len(namespaces) == 0 {
		return nil
	}
	return namespaces
}

// Seed adds objects (anything that marshals to a Kubernetes object) to a
// static cluster and marks the resource synced.
func (cl *Cluster) Seed(res Resource, objs ...any) error {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	k := scopeKey{res: res.Name}
	sc := cl.scopes[k]
	if sc == nil {
		sc = &scope{objs: map[string]json.RawMessage{}}
		cl.scopes[k] = sc
	}
	sc.synced = true
	for _, o := range objs {
		raw, err := json.Marshal(o)
		if err != nil {
			return err
		}
		var m struct {
			Metadata Meta `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		sc.objs[m.Metadata.Key()] = raw
	}
	return nil
}

// SetError marks a static cluster's resource as failing (tests).
func (cl *Cluster) SetError(res Resource, err error) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	k := scopeKey{res: res.Name}
	if cl.scopes[k] == nil {
		cl.scopes[k] = &scope{objs: map[string]json.RawMessage{}}
	}
	cl.scopes[k].err = err
}

// Client is the API client the cache reads through (nil for NewStatic).
func (cl *Cluster) Client() *Client { return cl.client }

// Limited returns the allowed namespaces, nil if all are.
func (cl *Cluster) Limited() []string { return cl.namespaces }

func (cl *Cluster) scopeKeys() []scopeKey {
	var keys []scopeKey
	for _, r := range Watched {
		if r.Namespaced && cl.namespaces != nil {
			for _, ns := range cl.namespaces {
				keys = append(keys, scopeKey{r.Name, ns})
			}
			continue
		}
		if r == ResNamespaces && cl.namespaces != nil {
			continue // the allowed namespaces are known
		}
		keys = append(keys, scopeKey{r.Name, ""})
	}
	return keys
}

// Run starts the watch loops; they end with ctx.
func (cl *Cluster) Run(ctx context.Context) {
	if cl.client == nil {
		return
	}
	byName := map[string]Resource{}
	for _, r := range Watched {
		byName[r.Name] = r
	}
	for _, k := range cl.scopeKeys() {
		go cl.loop(ctx, byName[k.res], k)
	}
}

// WaitSynced waits until every resource was listed once (or failed), or
// ctx ends. It reports whether all are synced.
func (cl *Cluster) WaitSynced(ctx context.Context) bool {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		cl.mu.RLock()
		done, all := true, true
		for _, sc := range cl.scopes {
			if !sc.synced {
				all = false
				if sc.err == nil {
					done = false
				}
			}
		}
		cl.mu.RUnlock()
		if done {
			return all
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
}

const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
)

func (cl *Cluster) loop(ctx context.Context, res Resource, k scopeKey) {
	backoff := minBackoff
	sleep := func() bool {
		t := time.NewTimer(backoff + time.Duration(rand.Int64N(int64(backoff/2))))
		defer t.Stop()
		backoff = min(backoff*2, maxBackoff)
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	for ctx.Err() == nil {
		rv, err := cl.relist(ctx, res, k)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			cl.setErr(k, err)
			if !sleep() {
				return
			}
			continue
		}
		backoff = minBackoff
		for ctx.Err() == nil {
			q := url.Values{"resourceVersion": {rv}, "timeoutSeconds": {strconv.Itoa(240 + rand.IntN(120))}}
			err := cl.client.Watch(ctx, res.Path(k.ns), q, func(ev WatchEvent) error {
				if v := cl.apply(k, ev); v != "" {
					rv = v
				}
				return nil
			})
			if err == nil {
				backoff = minBackoff
				continue
			}
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrGone) {
				break // list again
			}
			cl.setErr(k, err)
			if !sleep() {
				return
			}
		}
	}
}

// relist replaces the scope's content with a fresh list.
func (cl *Cluster) relist(ctx context.Context, res Resource, k scopeKey) (string, error) {
	items, rv, err := cl.client.List(ctx, res.Path(k.ns), nil)
	if err != nil {
		return "", err
	}
	objs := make(map[string]json.RawMessage, len(items))
	for _, raw := range items {
		var m struct {
			Metadata Meta `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", fmt.Errorf("decode %s: %w", res.Name, err)
		}
		objs[m.Metadata.Key()] = raw
	}
	cl.mu.Lock()
	sc := cl.scopes[k]
	if sc.err != nil {
		cl.log.Info("kubernetes watch recovered", "resource", res.Name, "namespace", k.ns)
	}
	sc.objs, sc.synced, sc.err = objs, true, nil
	cl.mu.Unlock()
	return rv, nil
}

// apply records one watch event and returns the resource version it
// carries.
func (cl *Cluster) apply(k scopeKey, ev WatchEvent) string {
	var m struct {
		Metadata Meta `json:"metadata"`
	}
	if err := json.Unmarshal(ev.Object, &m); err != nil {
		return ""
	}
	if ev.Type == Bookmark {
		return m.Metadata.ResourceVersion
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	sc := cl.scopes[k]
	sc.err = nil
	switch ev.Type {
	case Added, Modified:
		sc.objs[m.Metadata.Key()] = ev.Object
	case Deleted:
		delete(sc.objs, m.Metadata.Key())
	}
	return m.Metadata.ResourceVersion
}

func (cl *Cluster) setErr(k scopeKey, err error) {
	cl.mu.Lock()
	sc := cl.scopes[k]
	changed := sc.err == nil || sc.err.Error() != err.Error()
	sc.err = err
	cl.mu.Unlock()
	if changed {
		cl.log.Warn("kubernetes watch failing; retrying", "resource", k.res, "namespace", k.ns, "error", err)
	}
}

// allowed reports whether a namespace may be shown.
func (cl *Cluster) allowed(ns string) bool {
	return cl.namespaces == nil || ns == "" || slices.Contains(cl.namespaces, ns)
}

// Raw returns the objects of res in namespace ns ("" = all allowed),
// sorted by namespace and name.
func (cl *Cluster) Raw(res Resource, ns string) []json.RawMessage {
	cl.mu.RLock()
	defer cl.mu.RUnlock()
	type item struct {
		key string
		raw json.RawMessage
	}
	var items []item
	for k, sc := range cl.scopes {
		if k.res != res.Name || (k.ns != "" && ns != "" && k.ns != ns) {
			continue
		}
		for key, raw := range sc.objs {
			if res.Namespaced {
				objNS, _, _ := cutKey(key)
				if (ns != "" && objNS != ns) || !cl.allowed(objNS) {
					continue
				}
			}
			items = append(items, item{key, raw})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })
	out := make([]json.RawMessage, len(items))
	for i, it := range items {
		out[i] = it.raw
	}
	return out
}

// GetRaw returns one object (ns "" for cluster-scoped ones).
func (cl *Cluster) GetRaw(res Resource, ns, name string) (json.RawMessage, bool) {
	if res.Namespaced && !cl.allowed(ns) {
		return nil, false
	}
	key := name
	if res.Namespaced {
		key = ns + "/" + name
	}
	cl.mu.RLock()
	defer cl.mu.RUnlock()
	for k, sc := range cl.scopes {
		if k.res != res.Name {
			continue
		}
		if raw, ok := sc.objs[key]; ok {
			return raw, true
		}
	}
	return nil, false
}

func cutKey(key string) (ns, name string, ok bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:], true
		}
	}
	return "", key, false
}

// State tells whether resources are loaded.
type State struct {
	Synced bool   // every scope was listed at least once
	Err    string // the first current error, "" if none
}

// State reports the state of the given resources.
func (cl *Cluster) State(res ...Resource) State {
	cl.mu.RLock()
	defer cl.mu.RUnlock()
	st := State{Synced: true}
	for _, r := range res {
		for k, sc := range cl.scopes {
			if k.res != r.Name {
				continue
			}
			if !sc.synced {
				st.Synced = false
			}
			if sc.err != nil && st.Err == "" {
				st.Err = r.Name + ": " + sc.err.Error()
			}
		}
	}
	return st
}

// List decodes the objects of res in namespace ns ("" = all allowed).
// Objects that do not decode are skipped.
func List[T any](cl *Cluster, res Resource, ns string) []T {
	raws := cl.Raw(res, ns)
	out := make([]T, 0, len(raws))
	for _, raw := range raws {
		var v T
		if json.Unmarshal(raw, &v) == nil {
			out = append(out, v)
		}
	}
	return out
}

// Get decodes one object.
func Get[T any](cl *Cluster, res Resource, ns, name string) (*T, json.RawMessage, bool) {
	raw, ok := cl.GetRaw(res, ns, name)
	if !ok {
		return nil, nil, false
	}
	var v T
	if json.Unmarshal(raw, &v) != nil {
		return nil, nil, false
	}
	return &v, raw, true
}

// Namespaces returns the namespaces the UI offers: the allowed ones, or
// every namespace in the cluster.
func (cl *Cluster) Namespaces() []string {
	if cl.namespaces != nil {
		return cl.namespaces
	}
	var out []string
	for _, n := range List[Namespace](cl, ResNamespaces, "") {
		out = append(out, n.Metadata.Name)
	}
	return out
}

// Owner resolves the top-level controller of an object: a pod's
// ReplicaSet is followed to its Deployment, a Job to its CronJob. kind is
// "" for objects without a controller.
func (cl *Cluster) Owner(m Meta) (kind, name string) {
	ref := m.Controller()
	if ref == nil {
		return "", ""
	}
	var parent *OwnerRef
	switch ref.Kind {
	case KindReplicaSet:
		if rs, _, ok := Get[ReplicaSet](cl, ResReplicaSets, m.Namespace, ref.Name); ok {
			parent = rs.Metadata.Controller()
		}
	case KindJob:
		if j, _, ok := Get[Job](cl, ResJobs, m.Namespace, ref.Name); ok {
			parent = j.Metadata.Controller()
		}
	}
	if parent != nil {
		return parent.Kind, parent.Name
	}
	return ref.Kind, ref.Name
}

// Workloads returns the Deployments, StatefulSets, DaemonSets, CronJobs
// and Jobs in ns ("" = all allowed), sorted by namespace, kind and name.
func (cl *Cluster) Workloads(ns string) []Workload {
	var out []Workload
	for _, d := range List[Deployment](cl, ResDeployments, ns) {
		out = append(out, d.Workload())
	}
	for _, s := range List[StatefulSet](cl, ResStatefulSets, ns) {
		out = append(out, s.Workload())
	}
	for _, d := range List[DaemonSet](cl, ResDaemonSets, ns) {
		out = append(out, d.Workload())
	}
	for _, c := range List[CronJob](cl, ResCronJobs, ns) {
		out = append(out, c.Workload())
	}
	for _, j := range List[Job](cl, ResJobs, ns) {
		out = append(out, j.Workload())
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Meta.Namespace != b.Meta.Namespace {
			return a.Meta.Namespace < b.Meta.Namespace
		}
		if a.Kind != b.Kind {
			return kindOrder(a.Kind) < kindOrder(b.Kind)
		}
		return a.Meta.Name < b.Meta.Name
	})
	return out
}

func kindOrder(k string) int {
	switch k {
	case KindDeployment:
		return 0
	case KindStatefulSet:
		return 1
	case KindDaemonSet:
		return 2
	case KindCronJob:
		return 3
	}
	return 4
}

// Workload finds one workload by kind.
func (cl *Cluster) Workload(kind, ns, name string) (Workload, json.RawMessage, bool) {
	switch kind {
	case KindDeployment:
		if d, raw, ok := Get[Deployment](cl, ResDeployments, ns, name); ok {
			return d.Workload(), raw, true
		}
	case KindStatefulSet:
		if s, raw, ok := Get[StatefulSet](cl, ResStatefulSets, ns, name); ok {
			return s.Workload(), raw, true
		}
	case KindDaemonSet:
		if d, raw, ok := Get[DaemonSet](cl, ResDaemonSets, ns, name); ok {
			return d.Workload(), raw, true
		}
	case KindCronJob:
		if c, raw, ok := Get[CronJob](cl, ResCronJobs, ns, name); ok {
			return c.Workload(), raw, true
		}
	case KindJob:
		if j, raw, ok := Get[Job](cl, ResJobs, ns, name); ok {
			return j.Workload(), raw, true
		}
	}
	return Workload{}, nil, false
}

// WorkloadResources are the resources Workloads reads.
var WorkloadResources = []Resource{ResDeployments, ResStatefulSets, ResDaemonSets, ResCronJobs, ResJobs}

// MatchLabels reports whether labels has every key/value of selector; an
// empty selector matches nothing.
func MatchLabels(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}
