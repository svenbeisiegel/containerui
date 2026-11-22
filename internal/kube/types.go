package kube

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The structs below hold only the fields ContainerUI shows; the raw
// document is kept next to them for the YAML view.

// Meta is metav1.ObjectMeta.
type Meta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
	OwnerReferences   []OwnerRef        `json:"ownerReferences"`
}

// OwnerRef is metav1.OwnerReference.
type OwnerRef struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

// Controller returns the owner reference marked as controller, if any.
func (m Meta) Controller() *OwnerRef {
	for i := range m.OwnerReferences {
		if m.OwnerReferences[i].Controller {
			return &m.OwnerReferences[i]
		}
	}
	return nil
}

// Key is "namespace/name", or "name" for cluster-scoped objects.
func (m Meta) Key() string {
	if m.Namespace == "" {
		return m.Name
	}
	return m.Namespace + "/" + m.Name
}

// Condition is a status condition of any kind.
type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason"`
	Message            string    `json:"message"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// Namespace is a v1 Namespace.
type Namespace struct {
	Metadata Meta `json:"metadata"`
	Status   struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

// ---- pods ----

type Pod struct {
	Metadata Meta    `json:"metadata"`
	Spec     PodSpec `json:"spec"`
	Status   struct {
		Phase                 string            `json:"phase"`
		Reason                string            `json:"reason"`
		Message               string            `json:"message"`
		PodIP                 string            `json:"podIP"`
		HostIP                string            `json:"hostIP"`
		QOSClass              string            `json:"qosClass"`
		StartTime             *time.Time        `json:"startTime"`
		Conditions            []Condition       `json:"conditions"`
		InitContainerStatuses []ContainerStatus `json:"initContainerStatuses"`
		ContainerStatuses     []ContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

type PodSpec struct {
	NodeName           string            `json:"nodeName"`
	ServiceAccountName string            `json:"serviceAccountName"`
	RestartPolicy      string            `json:"restartPolicy"`
	PriorityClassName  string            `json:"priorityClassName"`
	HostNetwork        bool              `json:"hostNetwork"`
	InitContainers     []Container       `json:"initContainers"`
	Containers         []Container       `json:"containers"`
	Volumes            []Volume          `json:"volumes"`
	NodeSelector       map[string]string `json:"nodeSelector"`
}

type Container struct {
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	Command []string `json:"command"`
	Args    []string `json:"args"`
	Ports   []struct {
		Name          string `json:"name"`
		ContainerPort int    `json:"containerPort"`
		Protocol      string `json:"protocol"`
	} `json:"ports"`
	Resources struct {
		Requests map[string]string `json:"requests"`
		Limits   map[string]string `json:"limits"`
	} `json:"resources"`
	VolumeMounts []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
		SubPath   string `json:"subPath"`
		ReadOnly  bool   `json:"readOnly"`
	} `json:"volumeMounts"`
	ReadinessProbe *json.RawMessage `json:"readinessProbe"`
	LivenessProbe  *json.RawMessage `json:"livenessProbe"`
}

type Volume struct {
	Name                  string `json:"name"`
	PersistentVolumeClaim *struct {
		ClaimName string `json:"claimName"`
	} `json:"persistentVolumeClaim"`
	ConfigMap *struct {
		Name string `json:"name"`
	} `json:"configMap"`
	Secret *struct {
		SecretName string `json:"secretName"`
	} `json:"secret"`
	HostPath *struct {
		Path string `json:"path"`
	} `json:"hostPath"`
	EmptyDir  *json.RawMessage `json:"emptyDir"`
	Projected *json.RawMessage `json:"projected"`
}

// Source describes where a volume's data comes from.
func (v Volume) Source() string {
	switch {
	case v.PersistentVolumeClaim != nil:
		return "PVC " + v.PersistentVolumeClaim.ClaimName
	case v.ConfigMap != nil:
		return "ConfigMap " + v.ConfigMap.Name
	case v.Secret != nil:
		return "Secret " + v.Secret.SecretName
	case v.HostPath != nil:
		return "host path " + v.HostPath.Path
	case v.EmptyDir != nil:
		return "empty dir"
	case v.Projected != nil:
		return "projected"
	}
	return "other"
}

type ContainerStatus struct {
	Name         string         `json:"name"`
	Image        string         `json:"image"`
	Ready        bool           `json:"ready"`
	Started      *bool          `json:"started"`
	RestartCount int            `json:"restartCount"`
	State        ContainerState `json:"state"`
	LastState    ContainerState `json:"lastState"`
}

type ContainerState struct {
	Running *struct {
		StartedAt time.Time `json:"startedAt"`
	} `json:"running"`
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting"`
	Terminated *struct {
		Reason     string    `json:"reason"`
		Message    string    `json:"message"`
		ExitCode   int       `json:"exitCode"`
		StartedAt  time.Time `json:"startedAt"`
		FinishedAt time.Time `json:"finishedAt"`
	} `json:"terminated"`
}

// String is "running", "waiting: Reason" or "terminated: Reason (exit n)".
func (s ContainerState) String() string {
	switch {
	case s.Running != nil:
		return "running"
	case s.Waiting != nil:
		if s.Waiting.Reason != "" {
			return "waiting: " + s.Waiting.Reason
		}
		return "waiting"
	case s.Terminated != nil:
		r := s.Terminated.Reason
		if r == "" {
			r = "terminated"
		}
		return fmt.Sprintf("%s (exit %d)", r, s.Terminated.ExitCode)
	}
	return ""
}

// Status is the pod's status as kubectl shows it: the reason of a waiting
// or failed container (CrashLoopBackOff, Error, ...) before the phase.
func (p Pod) StatusText() string {
	if p.Metadata.DeletionTimestamp != nil {
		return "Terminating"
	}
	if p.Status.Reason != "" {
		return p.Status.Reason
	}
	for i, cs := range p.Status.InitContainerStatuses {
		switch {
		case cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0:
			continue
		case cs.State.Terminated != nil:
			return "Init:" + orDefault(cs.State.Terminated.Reason, "Error")
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != "PodInitializing":
			return "Init:" + cs.State.Waiting.Reason
		default:
			return fmt.Sprintf("Init:%d/%d", i, len(p.Spec.InitContainers))
		}
	}
	reason := p.Status.Phase
	running := false
	for _, cs := range p.Status.ContainerStatuses {
		switch {
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
			reason = cs.State.Waiting.Reason
		case cs.State.Terminated != nil && cs.State.Terminated.Reason != "":
			reason = cs.State.Terminated.Reason
		case cs.State.Terminated != nil:
			reason = fmt.Sprintf("ExitCode:%d", cs.State.Terminated.ExitCode)
		case cs.State.Running != nil && cs.Ready:
			running = true
		}
	}
	if reason == "Completed" && running {
		reason = "Running"
	}
	if reason == "" {
		reason = "Unknown"
	}
	return reason
}

// StatusClass colors the status: ok, warn, bad or muted (finished).
func (p Pod) StatusClass() string {
	switch s := p.StatusText(); {
	case s == "Running" && p.ReadyCount() == len(p.Spec.Containers):
		return "ok"
	case s == "Succeeded" || s == "Completed":
		return "muted"
	case s == "Running", s == "Pending", s == "ContainerCreating", s == "PodInitializing", s == "Terminating", strings.HasPrefix(s, "Init:") && !strings.Contains(s, "Err") && !strings.Contains(s, "BackOff"):
		return "warn"
	}
	return "bad"
}

// Finished reports whether the pod ran to completion or failed for good
// (a Job's pod).
func (p Pod) Finished() bool {
	return p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed"
}

// ReadyCount is the number of ready containers.
func (p Pod) ReadyCount() int {
	n := 0
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			n++
		}
	}
	return n
}

// Restarts sums the containers' restart counts.
func (p Pod) Restarts() int {
	n := 0
	for _, cs := range p.Status.ContainerStatuses {
		n += cs.RestartCount
	}
	return n
}

// LastRestart is when a container last restarted (its previous run
// ended), zero if none did.
func (p Pod) LastRestart() time.Time {
	var t time.Time
	for _, cs := range p.Status.ContainerStatuses {
		if lt := cs.LastState.Terminated; lt != nil && lt.FinishedAt.After(t) {
			t = lt.FinishedAt
		}
	}
	return t
}

// ContainerStatus returns the status of the named container.
func (p Pod) ContainerStatus(name string) *ContainerStatus {
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].Name == name {
			return &p.Status.ContainerStatuses[i]
		}
	}
	for i := range p.Status.InitContainerStatuses {
		if p.Status.InitContainerStatuses[i].Name == name {
			return &p.Status.InitContainerStatuses[i]
		}
	}
	return nil
}

// Claims lists the PVCs the pod mounts.
func (p Pod) Claims() []string {
	var out []string
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			out = append(out, v.PersistentVolumeClaim.ClaimName)
		}
	}
	return out
}

// ---- workloads ----

// Workload kinds.
const (
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindDaemonSet   = "DaemonSet"
	KindJob         = "Job"
	KindCronJob     = "CronJob"
	KindReplicaSet  = "ReplicaSet"
)

type LabelSelector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

type podTemplate struct {
	Metadata Meta    `json:"metadata"`
	Spec     PodSpec `json:"spec"`
}

type Deployment struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		Replicas *int          `json:"replicas"`
		Selector LabelSelector `json:"selector"`
		Template podTemplate   `json:"template"`
		Strategy struct {
			Type string `json:"type"`
		} `json:"strategy"`
	} `json:"spec"`
	Status struct {
		Replicas          int         `json:"replicas"`
		ReadyReplicas     int         `json:"readyReplicas"`
		UpdatedReplicas   int         `json:"updatedReplicas"`
		AvailableReplicas int         `json:"availableReplicas"`
		Conditions        []Condition `json:"conditions"`
	} `json:"status"`
}

type StatefulSet struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		Replicas             *int          `json:"replicas"`
		Selector             LabelSelector `json:"selector"`
		Template             podTemplate   `json:"template"`
		ServiceName          string        `json:"serviceName"`
		VolumeClaimTemplates []struct {
			Metadata Meta `json:"metadata"`
		} `json:"volumeClaimTemplates"`
		UpdateStrategy struct {
			Type string `json:"type"`
		} `json:"updateStrategy"`
	} `json:"spec"`
	Status struct {
		Replicas          int         `json:"replicas"`
		ReadyReplicas     int         `json:"readyReplicas"`
		UpdatedReplicas   int         `json:"updatedReplicas"`
		AvailableReplicas int         `json:"availableReplicas"`
		Conditions        []Condition `json:"conditions"`
	} `json:"status"`
}

type DaemonSet struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		Selector LabelSelector `json:"selector"`
		Template podTemplate   `json:"template"`
	} `json:"spec"`
	Status struct {
		DesiredNumberScheduled int         `json:"desiredNumberScheduled"`
		NumberReady            int         `json:"numberReady"`
		UpdatedNumberScheduled int         `json:"updatedNumberScheduled"`
		NumberAvailable        int         `json:"numberAvailable"`
		Conditions             []Condition `json:"conditions"`
	} `json:"status"`
}

type ReplicaSet struct {
	Metadata Meta `json:"metadata"`
}

type Job struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		Completions *int          `json:"completions"`
		Parallelism *int          `json:"parallelism"`
		Selector    LabelSelector `json:"selector"`
		Template    podTemplate   `json:"template"`
	} `json:"spec"`
	Status struct {
		Active         int         `json:"active"`
		Succeeded      int         `json:"succeeded"`
		Failed         int         `json:"failed"`
		StartTime      *time.Time  `json:"startTime"`
		CompletionTime *time.Time  `json:"completionTime"`
		Conditions     []Condition `json:"conditions"`
	} `json:"status"`
}

type CronJob struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		Schedule    string `json:"schedule"`
		Suspend     *bool  `json:"suspend"`
		JobTemplate struct {
			Spec struct {
				Template podTemplate `json:"template"`
			} `json:"spec"`
		} `json:"jobTemplate"`
	} `json:"spec"`
	Status struct {
		Active             []OwnerRef `json:"active"`
		LastScheduleTime   *time.Time `json:"lastScheduleTime"`
		LastSuccessfulTime *time.Time `json:"lastSuccessfulTime"`
	} `json:"status"`
}

// Workload is the common view of Deployments, StatefulSets, DaemonSets,
// Jobs and CronJobs.
type Workload struct {
	Kind       string
	Meta       Meta
	Desired    int // replicas / scheduled pods / completions
	Ready      int // ready replicas / succeeded completions
	Updated    int
	Available  int
	Images     []string
	Containers []Container
	Volumes    []Volume
	Selector   map[string]string
	Conditions []Condition
	// Extra holds kind-specific facts in display order (schedule,
	// strategy, service, ...).
	Extra []KV
	// Suspended: a CronJob that does not schedule jobs.
	Suspended bool
	Complete  bool // a Job that finished successfully
	Failed    bool // a Job that failed
}

// KV is one key-value pair for display.
type KV struct{ Key, Value string }

// Healthy reports whether the workload has what it wants.
func (w Workload) Healthy() bool {
	switch w.Kind {
	case KindJob:
		return !w.Failed
	case KindCronJob:
		return true
	}
	return w.Ready >= w.Desired && w.Updated >= w.Desired
}

// ReadyText is "ready/desired" ("succeeded/completions" for jobs).
func (w Workload) ReadyText() string {
	if w.Kind == KindCronJob {
		return ""
	}
	return fmt.Sprintf("%d/%d", w.Ready, w.Desired)
}

func images(cs []Container) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Image)
	}
	return out
}

func (d *Deployment) Workload() Workload {
	desired := 1
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	w := Workload{Kind: KindDeployment, Meta: d.Metadata, Desired: desired, Ready: d.Status.ReadyReplicas,
		Updated: d.Status.UpdatedReplicas, Available: d.Status.AvailableReplicas,
		Containers: d.Spec.Template.Spec.Containers, Volumes: d.Spec.Template.Spec.Volumes,
		Selector: d.Spec.Selector.MatchLabels, Conditions: d.Status.Conditions}
	if d.Spec.Strategy.Type != "" {
		w.Extra = append(w.Extra, KV{"Strategy", d.Spec.Strategy.Type})
	}
	w.Images = images(w.Containers)
	return w
}

func (s *StatefulSet) Workload() Workload {
	desired := 1
	if s.Spec.Replicas != nil {
		desired = *s.Spec.Replicas
	}
	w := Workload{Kind: KindStatefulSet, Meta: s.Metadata, Desired: desired, Ready: s.Status.ReadyReplicas,
		Updated: s.Status.UpdatedReplicas, Available: s.Status.AvailableReplicas,
		Containers: s.Spec.Template.Spec.Containers, Volumes: s.Spec.Template.Spec.Volumes,
		Selector: s.Spec.Selector.MatchLabels, Conditions: s.Status.Conditions}
	if s.Spec.ServiceName != "" {
		w.Extra = append(w.Extra, KV{"Service", s.Spec.ServiceName})
	}
	if s.Spec.UpdateStrategy.Type != "" {
		w.Extra = append(w.Extra, KV{"Update strategy", s.Spec.UpdateStrategy.Type})
	}
	var claims []string
	for _, t := range s.Spec.VolumeClaimTemplates {
		claims = append(claims, t.Metadata.Name)
	}
	if len(claims) > 0 {
		w.Extra = append(w.Extra, KV{"Volume claim templates", strings.Join(claims, ", ")})
	}
	w.Images = images(w.Containers)
	return w
}

func (d *DaemonSet) Workload() Workload {
	w := Workload{Kind: KindDaemonSet, Meta: d.Metadata, Desired: d.Status.DesiredNumberScheduled,
		Ready: d.Status.NumberReady, Updated: d.Status.UpdatedNumberScheduled, Available: d.Status.NumberAvailable,
		Containers: d.Spec.Template.Spec.Containers, Volumes: d.Spec.Template.Spec.Volumes,
		Selector: d.Spec.Selector.MatchLabels, Conditions: d.Status.Conditions}
	w.Images = images(w.Containers)
	return w
}

func (j *Job) Workload() Workload {
	completions := 1
	if j.Spec.Completions != nil {
		completions = *j.Spec.Completions
	}
	w := Workload{Kind: KindJob, Meta: j.Metadata, Desired: completions, Ready: j.Status.Succeeded,
		Updated: completions, Available: j.Status.Succeeded,
		Containers: j.Spec.Template.Spec.Containers, Volumes: j.Spec.Template.Spec.Volumes,
		Selector: j.Spec.Selector.MatchLabels, Conditions: j.Status.Conditions}
	for _, c := range j.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Complete":
			w.Complete = true
		case "Failed":
			w.Failed = true
		}
	}
	if j.Status.Active > 0 {
		w.Extra = append(w.Extra, KV{"Active pods", strconv.Itoa(j.Status.Active)})
	}
	if j.Status.Failed > 0 {
		w.Extra = append(w.Extra, KV{"Failed pods", strconv.Itoa(j.Status.Failed)})
	}
	if t := j.Status.CompletionTime; t != nil && j.Status.StartTime != nil {
		w.Extra = append(w.Extra, KV{"Duration", t.Sub(*j.Status.StartTime).Round(time.Second).String()})
	}
	w.Images = images(w.Containers)
	return w
}

func (c *CronJob) Workload() Workload {
	w := Workload{Kind: KindCronJob, Meta: c.Metadata, Ready: len(c.Status.Active),
		Containers: c.Spec.JobTemplate.Spec.Template.Spec.Containers,
		Volumes:    c.Spec.JobTemplate.Spec.Template.Spec.Volumes,
		Suspended:  c.Spec.Suspend != nil && *c.Spec.Suspend}
	w.Extra = append(w.Extra, KV{"Schedule", c.Spec.Schedule})
	if t := c.Status.LastScheduleTime; t != nil {
		w.Extra = append(w.Extra, KV{"Last schedule", t.Local().Format("2006-01-02 15:04:05")})
	}
	if t := c.Status.LastSuccessfulTime; t != nil {
		w.Extra = append(w.Extra, KV{"Last success", t.Local().Format("2006-01-02 15:04:05")})
	}
	w.Images = images(w.Containers)
	return w
}

// ---- services and ingresses ----

type Service struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		Type                  string            `json:"type"`
		ClusterIP             string            `json:"clusterIP"`
		ExternalIPs           []string          `json:"externalIPs"`
		LoadBalancerIP        string            `json:"loadBalancerIP"`
		ExternalTrafficPolicy string            `json:"externalTrafficPolicy"`
		Selector              map[string]string `json:"selector"`
		Ports                 []ServicePort     `json:"ports"`
	} `json:"spec"`
	Status struct {
		LoadBalancer LoadBalancerStatus `json:"loadBalancer"`
	} `json:"status"`
}

type ServicePort struct {
	Name       string          `json:"name"`
	Protocol   string          `json:"protocol"`
	Port       int             `json:"port"`
	TargetPort json.RawMessage `json:"targetPort"` // number or name
	NodePort   int             `json:"nodePort"`
}

// String is "443/TCP", "443:30443/TCP" with a node port, plus "→ target".
func (p ServicePort) String() string {
	proto := orDefault(p.Protocol, "TCP")
	s := strconv.Itoa(p.Port)
	if p.NodePort != 0 {
		s += ":" + strconv.Itoa(p.NodePort)
	}
	s += "/" + proto
	if t := strings.Trim(string(p.TargetPort), `"`); t != "" && t != strconv.Itoa(p.Port) {
		s += " → " + t
	}
	return s
}

type LoadBalancerStatus struct {
	Ingress []struct {
		IP       string `json:"ip"`
		Hostname string `json:"hostname"`
	} `json:"ingress"`
}

// Addresses lists the load balancer's IPs and host names.
func (l LoadBalancerStatus) Addresses() []string {
	var out []string
	for _, i := range l.Ingress {
		if i.IP != "" {
			out = append(out, i.IP)
		} else if i.Hostname != "" {
			out = append(out, i.Hostname)
		}
	}
	return out
}

// External lists the addresses the service is reachable at from outside.
func (s Service) External() []string {
	out := append([]string{}, s.Spec.ExternalIPs...)
	return append(out, s.Status.LoadBalancer.Addresses()...)
}

type Ingress struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		IngressClassName *string `json:"ingressClassName"`
		TLS              []struct {
			Hosts      []string `json:"hosts"`
			SecretName string   `json:"secretName"`
		} `json:"tls"`
		Rules []struct {
			Host string `json:"host"`
			HTTP *struct {
				Paths []struct {
					Path     string `json:"path"`
					PathType string `json:"pathType"`
					Backend  struct {
						Service *struct {
							Name string `json:"name"`
							Port struct {
								Number int    `json:"number"`
								Name   string `json:"name"`
							} `json:"port"`
						} `json:"service"`
					} `json:"backend"`
				} `json:"paths"`
			} `json:"http"`
		} `json:"rules"`
	} `json:"spec"`
	Status struct {
		LoadBalancer LoadBalancerStatus `json:"loadBalancer"`
	} `json:"status"`
}

// IngressRoute is one host/path → service rule.
type IngressRoute struct{ Host, Path, Service, Port string }

// Routes flattens the rules.
func (i Ingress) Routes() []IngressRoute {
	var out []IngressRoute
	for _, r := range i.Spec.Rules {
		if r.HTTP == nil {
			out = append(out, IngressRoute{Host: orDefault(r.Host, "*")})
			continue
		}
		for _, p := range r.HTTP.Paths {
			rt := IngressRoute{Host: orDefault(r.Host, "*"), Path: p.Path}
			if s := p.Backend.Service; s != nil {
				rt.Service = s.Name
				rt.Port = s.Port.Name
				if s.Port.Number != 0 {
					rt.Port = strconv.Itoa(s.Port.Number)
				}
			}
			out = append(out, rt)
		}
	}
	return out
}

// Class is the ingress class name, "" if unset.
func (i Ingress) Class() string {
	if i.Spec.IngressClassName == nil {
		return ""
	}
	return *i.Spec.IngressClassName
}

// ---- storage ----

type PVC struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		AccessModes      []string `json:"accessModes"`
		StorageClassName *string  `json:"storageClassName"`
		VolumeName       string   `json:"volumeName"`
		VolumeMode       string   `json:"volumeMode"`
		Resources        struct {
			Requests map[string]string `json:"requests"`
		} `json:"resources"`
	} `json:"spec"`
	Status struct {
		Phase    string            `json:"phase"`
		Capacity map[string]string `json:"capacity"`
	} `json:"status"`
}

// StorageClass is the PVC's storage class name, "" if unset.
func (p PVC) StorageClass() string {
	if p.Spec.StorageClassName == nil {
		return ""
	}
	return *p.Spec.StorageClassName
}

// Size is the bound capacity, else the requested size.
func (p PVC) Size() string {
	if s := p.Status.Capacity["storage"]; s != "" {
		return s
	}
	return p.Spec.Resources.Requests["storage"]
}

type PV struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		Capacity                      map[string]string `json:"capacity"`
		AccessModes                   []string          `json:"accessModes"`
		PersistentVolumeReclaimPolicy string            `json:"persistentVolumeReclaimPolicy"`
		StorageClassName              string            `json:"storageClassName"`
		VolumeMode                    string            `json:"volumeMode"`
		ClaimRef                      *struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"claimRef"`
		CSI *struct {
			Driver       string `json:"driver"`
			VolumeHandle string `json:"volumeHandle"`
		} `json:"csi"`
		HostPath *struct {
			Path string `json:"path"`
		} `json:"hostPath"`
		Local *struct {
			Path string `json:"path"`
		} `json:"local"`
		NFS *struct {
			Server string `json:"server"`
			Path   string `json:"path"`
		} `json:"nfs"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

// Claim is "namespace/name" of the bound claim, "" if none.
func (p PV) Claim() string {
	if p.Spec.ClaimRef == nil {
		return ""
	}
	return p.Spec.ClaimRef.Namespace + "/" + p.Spec.ClaimRef.Name
}

// Backend describes where the volume lives.
func (p PV) Backend() string {
	switch s := p.Spec; {
	case s.CSI != nil:
		return s.CSI.Driver
	case s.NFS != nil:
		return "nfs " + s.NFS.Server + ":" + s.NFS.Path
	case s.HostPath != nil:
		return "host path " + s.HostPath.Path
	case s.Local != nil:
		return "local " + s.Local.Path
	}
	return ""
}

type StorageClass struct {
	Metadata             Meta              `json:"metadata"`
	Provisioner          string            `json:"provisioner"`
	ReclaimPolicy        string            `json:"reclaimPolicy"`
	VolumeBindingMode    string            `json:"volumeBindingMode"`
	AllowVolumeExpansion *bool             `json:"allowVolumeExpansion"`
	Parameters           map[string]string `json:"parameters"`
}

// Default reports whether this is the cluster's default class.
func (s StorageClass) Default() bool {
	return s.Metadata.Annotations["storageclass.kubernetes.io/is-default-class"] == "true"
}

// Expandable reports whether volumes of the class can grow.
func (s StorageClass) Expandable() bool {
	return s.AllowVolumeExpansion != nil && *s.AllowVolumeExpansion
}

// ---- nodes ----

type Node struct {
	Metadata Meta `json:"metadata"`
	Spec     struct {
		Unschedulable bool `json:"unschedulable"`
		Taints        []struct {
			Key    string `json:"key"`
			Value  string `json:"value"`
			Effect string `json:"effect"`
		} `json:"taints"`
		PodCIDR string `json:"podCIDR"`
	} `json:"spec"`
	Status struct {
		Capacity    map[string]string `json:"capacity"`
		Allocatable map[string]string `json:"allocatable"`
		Conditions  []Condition       `json:"conditions"`
		Addresses   []struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"addresses"`
		NodeInfo struct {
			KubeletVersion          string `json:"kubeletVersion"`
			OSImage                 string `json:"osImage"`
			KernelVersion           string `json:"kernelVersion"`
			ContainerRuntimeVersion string `json:"containerRuntimeVersion"`
			Architecture            string `json:"architecture"`
			OperatingSystem         string `json:"operatingSystem"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

// Ready reports whether the node's Ready condition is true.
func (n Node) Ready() bool {
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

// nodeProblems are the standard node conditions that mean trouble when
// true. Distributions add their own conditions (RKE2: EtcdIsVoter), which
// are informational.
var nodeProblems = map[string]bool{"MemoryPressure": true, "DiskPressure": true, "PIDPressure": true, "NetworkUnavailable": true}

// Problems lists the node's problems: NotReady and the true pressure
// conditions.
func (n Node) Problems() []string {
	var out []string
	for _, c := range n.Status.Conditions {
		switch {
		case c.Type == "Ready" && c.Status != "True":
			out = append(out, "NotReady")
		case nodeProblems[c.Type] && c.Status == "True":
			out = append(out, c.Type)
		}
	}
	return out
}

// Roles lists the node's roles (node-role.kubernetes.io/<role> labels).
func (n Node) Roles() []string {
	var out []string
	for k := range n.Metadata.Labels {
		if r, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok && r != "" {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// InternalIP is the node's InternalIP address.
func (n Node) InternalIP() string {
	for _, a := range n.Status.Addresses {
		if a.Type == "InternalIP" {
			return a.Address
		}
	}
	return ""
}

// ---- events ----

type Event struct {
	Metadata       Meta `json:"metadata"`
	InvolvedObject struct {
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"involvedObject"`
	Reason             string     `json:"reason"`
	Message            string     `json:"message"`
	Type               string     `json:"type"`
	Count              int        `json:"count"`
	FirstTimestamp     *time.Time `json:"firstTimestamp"`
	LastTimestamp      *time.Time `json:"lastTimestamp"`
	EventTime          *time.Time `json:"eventTime"`
	ReportingComponent string     `json:"reportingComponent"`
	Source             struct {
		Component string `json:"component"`
		Host      string `json:"host"`
	} `json:"source"`
	Series *struct {
		Count            int        `json:"count"`
		LastObservedTime *time.Time `json:"lastObservedTime"`
	} `json:"series"`
}

// LastSeen is when the event last happened.
func (e Event) LastSeen() time.Time {
	switch {
	case e.Series != nil && e.Series.LastObservedTime != nil:
		return *e.Series.LastObservedTime
	case e.LastTimestamp != nil:
		return *e.LastTimestamp
	case e.EventTime != nil:
		return *e.EventTime
	case e.FirstTimestamp != nil:
		return *e.FirstTimestamp
	}
	return e.Metadata.CreationTimestamp
}

// Times is how often the event happened.
func (e Event) Times() int {
	if e.Series != nil && e.Series.Count > 0 {
		return e.Series.Count
	}
	return max(e.Count, 1)
}

// Component is who reported the event.
func (e Event) Component() string {
	return orDefault(e.Source.Component, e.ReportingComponent)
}

// Warning reports whether the event is of type Warning.
func (e Event) Warning() bool { return e.Type == "Warning" }

// ---- quantities ----

// ParseQuantity reads a Kubernetes quantity ("128Mi", "1.5G", "500m",
// "2") as a number of base units (bytes, cores). It returns false for
// what it cannot read.
func ParseQuantity(q string) (float64, bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return 0, false
	}
	suffixes := []struct {
		s string
		f float64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50}, {"Ei", 1 << 60},
		{"n", 1e-9}, {"u", 1e-6}, {"m", 1e-3}, {"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15}, {"E", 1e18},
	}
	f := 1.0
	for _, s := range suffixes {
		if strings.HasSuffix(q, s.s) {
			q, f = strings.TrimSuffix(q, s.s), s.f
			break
		}
	}
	v, err := strconv.ParseFloat(q, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v * f, true
}

// Bytes reads a memory or storage quantity as bytes (0 if unreadable).
func Bytes(q string) uint64 {
	v, ok := ParseQuantity(q)
	if !ok || v < 0 {
		return 0
	}
	return uint64(v)
}

// Cores reads a CPU quantity as cores (0 if unreadable).
func Cores(q string) float64 {
	v, _ := ParseQuantity(q)
	return v
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
