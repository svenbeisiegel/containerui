package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/svenbeisiegel/containerui/internal/kube"
)

// seedCluster builds a cluster like the RKE2 test appliance: three nodes,
// the osem StatefulSet with a crash-looping pod, a CronJob with a finished
// job pod, CoreDNS as a Deployment, a LoadBalancer service, Longhorn
// storage and a warning event.
func seedCluster(t *testing.T, namespaces []string) *kube.Cluster {
	t.Helper()
	ts := func(ago time.Duration) string { return time.Now().Add(-ago).UTC().Format(time.RFC3339) }
	obj := func(s string) json.RawMessage {
		s = strings.NewReplacer("@1h", ts(time.Hour), "@10m", ts(10*time.Minute), "@2m", ts(2*time.Minute), "@11d", ts(11*24*time.Hour)).Replace(s)
		if !json.Valid([]byte(s)) {
			t.Fatalf("invalid seed JSON: %s", s)
		}
		return json.RawMessage(s)
	}
	cl := kube.NewStatic(namespaces)
	seed := func(res kube.Resource, objs ...json.RawMessage) {
		args := make([]any, len(objs))
		for i, o := range objs {
			args[i] = o
		}
		if err := cl.Seed(res, args...); err != nil {
			t.Fatal(err)
		}
	}
	seed(kube.ResNamespaces,
		obj(`{"metadata":{"name":"osem","creationTimestamp":"@11d"}}`),
		obj(`{"metadata":{"name":"kube-system","creationTimestamp":"@11d"}}`))
	node := func(name, extra string) json.RawMessage {
		return obj(`{"metadata":{"name":"` + name + `","creationTimestamp":"@11d","labels":{"node-role.kubernetes.io/control-plane":"true","node-role.kubernetes.io/etcd":"true"}},
			"spec":{` + extra + `},
			"status":{"capacity":{"cpu":"6","memory":"16379432Ki","pods":"110"},"allocatable":{"cpu":"6","memory":"16379432Ki","pods":"110"},
			"conditions":[{"type":"MemoryPressure","status":"False"},{"type":"EtcdIsVoter","status":"True","reason":"MemberNotLearner"},{"type":"Ready","status":"True","reason":"KubeletReady"}],
			"addresses":[{"type":"InternalIP","address":"192.168.21.10"}],
			"nodeInfo":{"kubeletVersion":"v1.36.6+rke2r1","osImage":"OSEM Container Appliance 2026.11-6","kernelVersion":"7.3.3-1-default","containerRuntimeVersion":"containerd://2.3.7-k3s1","architecture":"amd64"}}}`)
	}
	seed(kube.ResNodes, node("kube-1", ""), node("kube-2", ""), node("kube-3", `"unschedulable":true`))
	seed(kube.ResStatefulSets, obj(`{"metadata":{"name":"wpi","namespace":"osem","uid":"sts-wpi","creationTimestamp":"@11d","labels":{"app":"wpi"}},
		"spec":{"replicas":3,"serviceName":"wpi","selector":{"matchLabels":{"app":"wpi"}},
		"template":{"spec":{"containers":[{"name":"couchdb","image":"osem/couchdb:3.4","ports":[{"containerPort":5984}]},{"name":"wpi","image":"osem/wpi:2.5"}]}},
		"volumeClaimTemplates":[{"metadata":{"name":"osemdb-volume"}}]},
		"status":{"replicas":3,"readyReplicas":2,"updatedReplicas":3,"availableReplicas":2}}`))
	seed(kube.ResDeployments, obj(`{"metadata":{"name":"coredns","namespace":"kube-system","uid":"dep-coredns","creationTimestamp":"@11d"},
		"spec":{"replicas":1,"selector":{"matchLabels":{"k8s-app":"dns"}},"template":{"spec":{"containers":[{"name":"coredns","image":"rancher/coredns:1.12"}]}}},
		"status":{"replicas":1,"readyReplicas":1,"updatedReplicas":1,"availableReplicas":1}}`))
	seed(kube.ResReplicaSets, obj(`{"metadata":{"name":"coredns-7bf","namespace":"kube-system","ownerReferences":[{"kind":"Deployment","name":"coredns","controller":true}]}}`))
	seed(kube.ResCronJobs, obj(`{"metadata":{"name":"lifecycle-script","namespace":"osem","creationTimestamp":"@11d"},
		"spec":{"schedule":"* * * * *","jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"script","image":"osem/lifecycle:1"}]}}}}},
		"status":{"lastScheduleTime":"@2m"}}`))
	seed(kube.ResJobs, obj(`{"metadata":{"name":"lifecycle-script-29922990","namespace":"osem","creationTimestamp":"@2m","ownerReferences":[{"kind":"CronJob","name":"lifecycle-script","controller":true}]},
		"spec":{"completions":1,"template":{"spec":{"containers":[{"name":"script","image":"osem/lifecycle:1"}]}}},
		"status":{"succeeded":1,"startTime":"@2m","completionTime":"@2m","conditions":[{"type":"Complete","status":"True"}]}}`))
	seed(kube.ResPods,
		obj(`{"metadata":{"name":"wpi-0","namespace":"osem","creationTimestamp":"@11d","labels":{"app":"wpi"},"ownerReferences":[{"kind":"StatefulSet","name":"wpi","controller":true}],
			"managedFields":[{"manager":"kubelet"}]},
			"spec":{"nodeName":"kube-1","containers":[{"name":"couchdb","image":"osem/couchdb:3.4"},{"name":"wpi","image":"osem/wpi:2.5"}],
			"volumes":[{"name":"osemdb-volume","persistentVolumeClaim":{"claimName":"osemdb-volume-wpi-0"}}]},
			"status":{"phase":"Running","podIP":"10.42.0.7","conditions":[{"type":"Ready","status":"True"}],
			"containerStatuses":[{"name":"couchdb","ready":true,"state":{"running":{"startedAt":"@11d"}}},{"name":"wpi","ready":true,"state":{"running":{"startedAt":"@11d"}}}]}}`),
		obj(`{"metadata":{"name":"wpi-1","namespace":"osem","creationTimestamp":"@11d","labels":{"app":"wpi"},"ownerReferences":[{"kind":"StatefulSet","name":"wpi","controller":true}]},
			"spec":{"nodeName":"kube-2","containers":[{"name":"couchdb","image":"osem/couchdb:3.4"},{"name":"wpi","image":"osem/wpi:2.5"}]},
			"status":{"phase":"Running","podIP":"10.42.1.7",
			"containerStatuses":[{"name":"couchdb","ready":true,"state":{"running":{"startedAt":"@11d"}}},
			{"name":"wpi","ready":false,"restartCount":7,"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"reason":"Error","exitCode":1,"finishedAt":"@10m"}}}]}}`),
		obj(`{"metadata":{"name":"lifecycle-script-29922990-c7fmz","namespace":"osem","creationTimestamp":"@2m","ownerReferences":[{"kind":"Job","name":"lifecycle-script-29922990","controller":true}]},
			"spec":{"nodeName":"kube-1","containers":[{"name":"script","image":"osem/lifecycle:1"}]},
			"status":{"phase":"Succeeded","containerStatuses":[{"name":"script","state":{"terminated":{"reason":"Completed","exitCode":0}}}]}}`),
		obj(`{"metadata":{"name":"coredns-7bf-x2k","namespace":"kube-system","creationTimestamp":"@11d","labels":{"k8s-app":"dns"},"ownerReferences":[{"kind":"ReplicaSet","name":"coredns-7bf","controller":true}]},
			"spec":{"nodeName":"kube-3","containers":[{"name":"coredns","image":"rancher/coredns:1.12"}]},
			"status":{"phase":"Running","containerStatuses":[{"name":"coredns","ready":true,"state":{"running":{"startedAt":"@11d"}}}]}}`))
	seed(kube.ResServices, obj(`{"metadata":{"name":"wpi-external","namespace":"osem","creationTimestamp":"@11d"},
		"spec":{"type":"LoadBalancer","clusterIP":"10.35.220.46","selector":{"app":"wpi"},"ports":[{"name":"http","protocol":"TCP","port":80,"targetPort":8080,"nodePort":19565}]},
		"status":{"loadBalancer":{"ingress":[{"ip":"192.168.21.31"}]}}}`))
	seed(kube.ResIngresses)
	seed(kube.ResPVCs, obj(`{"metadata":{"name":"osemdb-volume-wpi-0","namespace":"osem","creationTimestamp":"@11d"},
		"spec":{"accessModes":["ReadWriteOnce"],"storageClassName":"longhorn-wpi","volumeName":"pvc-c9dbfa29","resources":{"requests":{"storage":"225Gi"}}},
		"status":{"phase":"Bound","capacity":{"storage":"225Gi"}}}`))
	seed(kube.ResPVs, obj(`{"metadata":{"name":"pvc-c9dbfa29","creationTimestamp":"@11d"},
		"spec":{"capacity":{"storage":"225Gi"},"accessModes":["ReadWriteOnce"],"persistentVolumeReclaimPolicy":"Delete","storageClassName":"longhorn-wpi",
		"claimRef":{"namespace":"osem","name":"osemdb-volume-wpi-0"},"csi":{"driver":"driver.longhorn.io","volumeHandle":"pvc-c9dbfa29"}},
		"status":{"phase":"Bound"}}`))
	seed(kube.ResStorageClasses, obj(`{"metadata":{"name":"longhorn","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}},
		"provisioner":"driver.longhorn.io","reclaimPolicy":"Delete","volumeBindingMode":"Immediate","allowVolumeExpansion":true}`))
	seed(kube.ResEvents,
		obj(`{"metadata":{"name":"wpi-1.ev1","namespace":"osem"},"involvedObject":{"kind":"Pod","namespace":"osem","name":"wpi-1"},
			"reason":"BackOff","message":"Back-off restarting failed container wpi","type":"Warning","count":42,"lastTimestamp":"@2m","source":{"component":"kubelet"}}`),
		obj(`{"metadata":{"name":"lifecycle.ev2","namespace":"osem"},"involvedObject":{"kind":"CronJob","namespace":"osem","name":"lifecycle-script"},
			"reason":"SuccessfulCreate","message":"Created job lifecycle-script-29922990","type":"Normal","count":1,"lastTimestamp":"@2m"}`))
	return cl
}

func newKubeEnv(t *testing.T, namespaces []string) *env {
	t.Helper()
	cl := seedCluster(t, namespaces)
	e := newEnvWith(t, func(o *Options) {
		o.Runtime = nil
		o.Kube = cl
	})
	e.login()
	return e
}

func expectBody(t *testing.T, w interface {
	Result() *http.Response
}, body string, code int, want ...string) {
	t.Helper()
	if w.Result().StatusCode != code {
		t.Fatalf("status %d, want %d:\n%s", w.Result().StatusCode, code, body)
	}
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("body lacks %q", s)
		}
	}
}

func TestKubePagesRender(t *testing.T) {
	e := newKubeEnv(t, nil)
	for path, want := range map[string][]string{
		"/":                               {"nodes ready", "2/3", "workloads healthy", "Needs attention", "wpi-1", "CrashLoopBackOff", "BackOff", "cordoned", `href="/workloads"`, "Cluster events"},
		"/workloads":                      {"wpi", "StatefulSet", "coredns", "Deployment", "lifecycle-script", "* * * * *", "1 jobs kept"},
		"/pods":                           {"wpi-0", "wpi-1", "coredns-7bf-x2k", "1 finished pod", "/workloads/kube-system/Deployment/coredns"},
		"/pods?finished=1":                {"lifecycle-script-29922990-c7fmz", "Completed", "Hide finished pods"},
		"/services":                       {"wpi-external", "LoadBalancer", "192.168.21.31", "80:19565/TCP → 8080", "1/2"},
		"/storage":                        {"osemdb-volume-wpi-0", "225.0 GiB", "longhorn-wpi", "wpi-0", "driver.longhorn.io", "default"},
		"/nodes":                          {"kube-1", "kube-3", "control-plane, etcd", "v1.36.6", "15.6 GiB", "cordoned"},
		"/cluster-events":                 {"Back-off restarting failed container wpi", "SuccessfulCreate", "42"},
		"/cluster-events?type=warning":    {"BackOff"},
		"/pods/osem/wpi-1":                {"CrashLoopBackOff", "Error (exit 1)", "wpi", "StatefulSet", "Back-off restarting", "kube-2"},
		"/pods/osem/wpi-0":                {"osemdb-volume-wpi-0", "/storage/pvc/osem/osemdb-volume-wpi-0", "YAML"},
		"/workloads/osem/StatefulSet/wpi": {"2/3", "Volume claim templates", "osemdb-volume", "wpi-0", "wpi-1", "osem/couchdb:3.4"},
		"/workloads/osem/CronJob/lifecycle-script":      {"* * * * *", "lifecycle-script-29922990", "complete", "SuccessfulCreate"},
		"/workloads/osem/Job/lifecycle-script-29922990": {"Succeeded", "lifecycle-script-29922990-c7fmz", "lifecycle-script"},
		"/workloads/kube-system/Deployment/coredns":     {"1/1", "coredns-7bf-x2k"},
		"/services/osem/wpi-external":                   {"19565", "wpi-0", "wpi-1", "matching the selector"},
		"/storage/pvc/osem/osemdb-volume-wpi-0":         {"pvc-c9dbfa29", "driver.longhorn.io", "wpi-0"},
		"/nodes/kube-1":                                 {"containerd://2.3.7-k3s1", "wpi-0", "MemoryPressure", "15.6 GiB"},
		"/system":                                       {"System"},
	} {
		w := e.do("GET", path, nil)
		expectBody(t, w, w.Body.String(), http.StatusOK, want...)
		if t.Failed() {
			t.Fatalf("page %s", path)
		}
	}
	for _, path := range []string{"/partials/dashboard", "/partials/workloads", "/partials/pods", "/partials/services", "/partials/storage", "/partials/nodes", "/partials/cluster-events"} {
		w := e.do("GET", path, nil, htmx)
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "<html") {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
}

func TestKubeNoRuntimeRoutes(t *testing.T) {
	e := newKubeEnv(t, nil)
	for _, path := range []string{"/containers", "/images", "/volumes", "/networks", "/compose"} {
		if w := e.do("GET", path, nil); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404 in kubernetes mode", path, w.Code)
		}
	}
	body := e.do("GET", "/pods", nil).Body.String()
	if strings.Contains(body, `href="/containers"`) || !strings.Contains(body, `title="Kubernetes cluster">kubernetes</span>`) {
		t.Error("navigation must show the cluster pages and the kubernetes badge")
	}
}

func TestKubeNamespacePicker(t *testing.T) {
	e := newKubeEnv(t, nil)
	w := e.do("GET", "/pods?ns=osem", nil)
	body := w.Body.String()
	if !strings.Contains(body, "wpi-0") || strings.Contains(body, "coredns-7bf-x2k") {
		t.Fatal("ns=osem must show only osem pods")
	}
	if !strings.Contains(body, `<option value="osem" selected>`) {
		t.Error("osem not selected in the picker")
	}
	var nsC *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == nsCookie {
			nsC = c
		}
	}
	if nsC == nil || nsC.Value != "osem" {
		t.Fatalf("namespace cookie = %+v", nsC)
	}
	// The cookie applies to the other list pages.
	body = e.do("GET", "/workloads", nil, func(r *http.Request) { r.AddCookie(nsC) }).Body.String()
	if strings.Contains(body, "coredns") || !strings.Contains(body, "wpi") {
		t.Error("remembered namespace not applied")
	}
	// Unknown namespaces fall back to all.
	body = e.do("GET", "/pods?ns=nope", nil).Body.String()
	if !strings.Contains(body, "coredns-7bf-x2k") {
		t.Error("unknown namespace must show all")
	}
}

func TestKubeLimitedNamespaces(t *testing.T) {
	e := newKubeEnv(t, []string{"osem"})
	body := e.do("GET", "/pods", nil).Body.String()
	if strings.Contains(body, "coredns") || !strings.Contains(body, "wpi-0") {
		t.Error("kube-system must be hidden when only osem is allowed")
	}
	if w := e.do("GET", "/workloads/kube-system/Deployment/coredns", nil); w.Code != http.StatusNotFound {
		t.Errorf("a workload outside the allowed namespaces: %d, want 404", w.Code)
	}
	if strings.Contains(e.do("GET", "/", nil).Body.String(), "coredns") {
		t.Error("dashboard shows a hidden namespace")
	}
}

func TestKubeDetailMissingAndYAML(t *testing.T) {
	e := newKubeEnv(t, nil)
	w := e.do("GET", "/pods/osem/gone", nil)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "was not found") {
		t.Errorf("missing pod: %d", w.Code)
	}
	body := e.do("GET", "/pods/osem/wpi-0", nil).Body.String()
	if strings.Contains(body, "managedFields") || !strings.Contains(body, "nodeName: kube-1") {
		t.Error("YAML must be block style without managed fields")
	}
	if i, j := strings.Index(body, "metadata:"), strings.Index(body, "spec:"); i < 0 || j < i {
		t.Error("YAML must keep the API server's key order")
	}
}

func TestKubeStateShown(t *testing.T) {
	cl := seedCluster(t, nil)
	cl.SetError(kube.ResPods, &kube.Error{Method: "GET", Path: "/api/v1/pods", Status: 403, Reason: "Forbidden", Message: "pods is forbidden"})
	e := newEnvWith(t, func(o *Options) { o.Runtime, o.Kube = nil, cl })
	e.login()
	if body := e.do("GET", "/pods", nil).Body.String(); !strings.Contains(body, "pods is forbidden") {
		t.Error("a failing watch must be shown")
	}
}

// The default config says "namespaces: []": an empty list means all
// namespaces, not none.
func TestKubeEmptyNamespaceListMeansAll(t *testing.T) {
	e := newKubeEnv(t, []string{})
	body := e.do("GET", "/pods", nil).Body.String()
	if !strings.Contains(body, "wpi-0") || !strings.Contains(body, "coredns-7bf-x2k") {
		t.Error("an empty namespace list must show every namespace")
	}
	if dash := e.do("GET", "/", nil).Body.String(); strings.Contains(dash, "(limited)") {
		t.Error("an empty namespace list must not count as limited")
	}
}

func TestKubeDistributionConditionsAreNoProblem(t *testing.T) {
	e := newKubeEnv(t, nil)
	if body := e.do("GET", "/nodes", nil).Body.String(); strings.Contains(body, `badge warn">EtcdIsVoter`) {
		t.Error("RKE2's EtcdIsVoter condition must not be shown as a problem")
	}
}
