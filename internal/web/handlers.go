package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/svenbeisiegel/containerui/internal/jobs"
	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// ---- dashboard ----

type dashboardView struct {
	Info       runtime.Info
	InfoErr    string
	Containers []runtime.Container // all containers
	Running    []runtime.Container
	ContErr    string
	Images     int // counts for the cards
	Volumes    int
	Networks   int
	Projects   int // compose projects, if supported
	Caps       runtime.Capabilities
	Updated    time.Time
	Metrics    *metricsView            // nil: the runtime reports no resource usage
	Current    map[string]currentStats // latest values by container ID
}

// loadDashboard queries everything concurrently; a failing section only
// shows its own error. Showing the dashboard keeps the metrics sampler
// running (see metricsSampler).
func (s *Server) loadDashboard(ctx context.Context) dashboardView {
	v := dashboardView{Caps: s.caps(), Updated: time.Now()}
	if s.metrics != nil && v.Caps.Stats {
		s.metrics.touch()
	}
	var wg sync.WaitGroup
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	run(func() {
		var err error
		if v.Info, err = s.rt.Info(ctx); err != nil {
			v.InfoErr = userError(err)
		}
	})
	run(func() {
		cs, err := s.rt.ListContainers(ctx, runtime.ListContainersOpts{All: true})
		if err != nil {
			v.ContErr = userError(err)
			return
		}
		v.Containers = cs
		for _, c := range cs {
			if c.State.IsRunning() {
				v.Running = append(v.Running, c)
			}
		}
	})
	run(func() {
		if images, err := s.rt.ListImages(ctx); err == nil {
			v.Images = len(images)
		}
	})
	if v.Caps.Volumes {
		run(func() {
			if vols, err := s.rt.ListVolumes(ctx); err == nil {
				v.Volumes = len(vols)
			}
		})
	}
	if v.Caps.Networks {
		run(func() {
			if nets, err := s.rt.ListNetworks(ctx); err == nil {
				v.Networks = len(nets)
			}
		})
	}
	if _, ok := s.composer(); ok {
		run(func() {
			if ps, err := s.loadProjects(ctx); err == nil {
				v.Projects = len(ps)
			}
		})
	}
	wg.Wait()
	if s.metrics != nil && v.Caps.Stats {
		snap := s.metrics.snapshot()
		now := time.Now()
		v.Metrics = s.metrics.buildMetricsView(snap, now)
		v.Current = currentFor(v.Running, snap, now, 3*s.metrics.interval)
	}
	return v
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, "dashboard", s.newPage(r, "Dashboard", "dashboard", s.loadDashboard(r.Context())))
}

func (s *Server) handleDashboardPartial(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, "dashboard_body", s.loadDashboard(r.Context()))
}

// ---- containers ----

type containersView struct {
	Containers []runtime.Container
	Err        string
	Flash      *flash
	Caps       runtime.Capabilities
	CSRF       string
}

func (s *Server) loadContainers(r *http.Request, f *flash) containersView {
	v := containersView{Flash: f, Caps: s.caps(), CSRF: sessionFrom(r).CSRF}
	cs, err := s.rt.ListContainers(r.Context(), runtime.ListContainersOpts{All: true})
	if err != nil {
		v.Err = userError(err)
	}
	v.Containers = cs
	return v
}

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	s.render(w, "containers", s.newPage(r, "Containers", "containers", s.loadContainers(r, nil)))
}

func (s *Server) handleContainersPartial(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, "containers_table", s.loadContainers(r, nil))
}

type containerDetailView struct {
	ID        string
	Container *runtime.Container
	Inspect   string
	Err       string
	Caps      runtime.Capabilities
	CSRF      string
	Terminal  *terminalView // nil if the runtime has no terminal support
	// Project is the compose project managing the container ("" if none).
	Project string
	// CanEdit: the container can be edited (recreated).
	CanEdit bool
	// Updates: show the image update check.
	Updates bool
	// Job is the running or recent recreate job of the container.
	Job *recreateJobView
}

func (s *Server) handleContainerDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v := containerDetailView{ID: id, Caps: s.caps(), CSRF: sessionFrom(r).CSRF}
	if cs, err := s.rt.ListContainers(r.Context(), runtime.ListContainersOpts{All: true}); err == nil {
		for i := range cs {
			if cs[i].ID == id || cs[i].Name == id {
				v.Container = &cs[i]
			}
		}
	}
	if v.Caps.Inspect {
		raw, err := s.rt.InspectContainer(r.Context(), id)
		if err != nil {
			v.Err = userError(err)
		} else {
			v.Inspect = prettyJSON(raw)
		}
	}
	name := id
	if v.Container != nil {
		name = v.Container.Name
	}
	if j := s.recreateJob(name); j != nil {
		v.Job = &recreateJobView{Name: name, Job: j}
	}
	if v.Container == nil && v.Inspect == "" {
		if v.Job != nil {
			// Between removing the old and creating the new container.
			v.Err = ""
		} else {
			w.WriteHeader(http.StatusNotFound)
			if v.Err == "" {
				v.Err = "Container " + id + " not found."
			}
		}
	}
	title := id
	if v.Container != nil {
		title = v.Container.Name
		v.Project = v.Container.Labels[runtime.LabelProject]
		v.CanEdit = v.Caps.Inspect && v.Project == ""
		v.Updates = s.updates != nil
		if v.Caps.Terminal {
			// Running containers load the shell check asynchronously.
			v.Terminal = &terminalView{ID: v.Container.ID, Name: v.Container.Name, State: "stopped", CSRF: v.CSRF}
			if v.Container.State == runtime.StateRunning {
				v.Terminal.State = "checking"
			}
		}
	}
	s.render(w, "container_detail", s.newPage(r, "Container "+title, "containers", v))
}

// prettyJSON indents inspect output; invalid JSON is returned as is.
func prettyJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") != nil {
		return string(raw)
	}
	return buf.String()
}

func (s *Server) handleContainerAction(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	name := r.PostFormValue("name")
	if name == "" {
		name = shortID(id)
	}
	ctx := r.Context()
	var err error
	var done string
	switch action {
	case "start":
		err, done = s.rt.StartContainer(ctx, id), "started"
	case "stop":
		err, done = s.rt.StopContainer(ctx, id, nil), "stopped"
	case "restart":
		if !s.caps().Restart {
			err = runtime.ErrUnsupported
		} else {
			err, done = s.rt.RestartContainer(ctx, id, nil), "restarted"
		}
	case "delete":
		err, done = s.rt.RemoveContainer(ctx, id, r.PostFormValue("force") == "true"), "deleted"
	default:
		http.NotFound(w, r)
		return
	}
	var f *flash
	if err != nil {
		s.log.Warn("container action failed", "action", action, "id", id, "user", sessionFrom(r).Username, "error", err)
		f = errFlash(err)
		if action == "delete" && errors.Is(err, runtime.ErrConflict) {
			f.Msg += " Stop the container first or use Force delete."
		}
	} else {
		s.log.Info("container action", "action", action, "id", id, "user", sessionFrom(r).Username)
		f = okFlash("Container %s %s.", name, done)
	}

	// Detail page and non-JS clients get a redirect; the list gets a fresh table.
	if !isHTMX(r) || r.PostFormValue("view") == "detail" {
		sessionFrom(r).SetFlash(f.Msg)
		to := "/containers"
		if r.PostFormValue("view") == "detail" && (action != "delete" || err != nil) {
			to = "/containers/" + id
		}
		// A compose project page lists its containers with these actions.
		if ret := r.PostFormValue("return"); composeReturnRe.MatchString(ret) {
			to = ret
		}
		redirect(w, r, to)
		return
	}
	s.renderPartial(w, "containers_table", s.loadContainers(r, f))
}

type containerForm struct {
	Image, Name, Cmd, Env, Ports, Volumes, Network, Pull string
	Start                                                bool
	// Aliases and IP of the first network; more networks, one per line:
	// "name [alias=a,b] [ip=x.x.x.x]".
	NetworkAliases, NetworkIP, ExtraNetworks string

	// Advanced options; the raw text is kept for re-rendering.
	TTY, Interactive, AutoRemove, NoHealthcheck, Privileged           bool
	CPUs, Memory, Hostname, User, DNS, DNSSearch, DNSOptions, Ulimits string
	Restart, Entrypoint, WorkDir, Labels, ExtraHosts, CapAdd, CapDrop string
}

// AdvancedChanged reports whether any advanced option differs from its
// default, in which case the collapsed section is rendered open.
func (f containerForm) AdvancedChanged() bool {
	return !f.TTY || !f.Interactive || f.AutoRemove || f.NoHealthcheck || f.Privileged ||
		f.CPUs != "" || f.Memory != "" || f.Hostname != "" || f.User != "" ||
		f.DNS != "" || f.DNSSearch != "" || f.DNSOptions != "" || f.Ulimits != "" ||
		f.Entrypoint != "" || f.WorkDir != "" || f.ExtraHosts != "" || f.CapAdd != "" || f.CapDrop != ""
}

// RestartPolicies are the choices of the restart policy select; a value
// such as on-failure:3 is added by the template.
var RestartPolicies = []string{"no", "always", "unless-stopped", "on-failure"}

func (f containerForm) RestartPolicies() []string {
	if f.Restart != "" && !slices.Contains(RestartPolicies, f.Restart) {
		return append(slices.Clone(RestartPolicies), f.Restart)
	}
	return RestartPolicies
}

// containerEdit describes the container an edit form recreates.
type containerEdit struct {
	ID, Name   string
	Running    bool
	AutoRemove bool
	Lost       []string // settings that cannot be carried over
}

type containerNewView struct {
	Form     containerForm
	Err      string
	Images   []runtime.Image
	Networks []runtime.Network
	Edit     *containerEdit // nil: create a new container
}

func (s *Server) newContainerView(ctx context.Context, f containerForm, err string) containerNewView {
	v := containerNewView{Form: f, Err: err}
	v.Images, _ = s.rt.ListImages(ctx)
	if s.caps().Networks {
		v.Networks, _ = s.rt.ListNetworks(ctx)
	}
	return v
}

func (s *Server) handleContainerNew(w http.ResponseWriter, r *http.Request) {
	f := containerForm{Image: r.URL.Query().Get("image"), Pull: string(runtime.PullMissing), Start: true, TTY: true, Interactive: true}
	s.render(w, "container_new", s.newPage(r, "Create container", "containers", s.newContainerView(r.Context(), f, "")))
}

// parseContainerForm reads the create / edit form.
func parseContainerForm(r *http.Request) containerForm {
	v := func(name string) string { return strings.TrimSpace(r.PostFormValue(name)) }
	on := func(name string) bool { return r.PostFormValue(name) == "on" }
	return containerForm{
		Image:          v("image"),
		Name:           v("name"),
		Cmd:            r.PostFormValue("cmd"),
		Env:            r.PostFormValue("env"),
		Ports:          r.PostFormValue("ports"),
		Volumes:        r.PostFormValue("volumes"),
		Network:        r.PostFormValue("network"),
		NetworkAliases: v("network_aliases"),
		NetworkIP:      v("network_ip"),
		ExtraNetworks:  r.PostFormValue("extra_networks"),
		Pull:           r.PostFormValue("pull"),
		Start:          on("start"),

		TTY:           on("tty"),
		Interactive:   on("interactive"),
		AutoRemove:    on("rm"),
		NoHealthcheck: on("no_healthcheck"),
		Privileged:    on("privileged"),
		CPUs:          v("cpus"),
		Memory:        v("memory"),
		Hostname:      v("hostname"),
		User:          v("user"),
		DNS:           v("dns"),
		DNSSearch:     v("dns_search"),
		DNSOptions:    v("dns_option"),
		Ulimits:       v("ulimit"),
		Restart:       v("restart"),
		Entrypoint:    r.PostFormValue("entrypoint"),
		WorkDir:       v("workdir"),
		Labels:        r.PostFormValue("labels"),
		ExtraHosts:    v("add_host"),
		CapAdd:        v("cap_add"),
		CapDrop:       v("cap_drop"),
	}
}

func (s *Server) handleContainerCreate(w http.ResponseWriter, r *http.Request) {
	f := parseContainerForm(r)
	opts, err := f.toOpts()
	if err == nil {
		var id string
		id, err = s.rt.CreateContainer(r.Context(), opts)
		if err == nil {
			s.log.Info("container created", "id", id, "image", opts.Image, "user", sessionFrom(r).Username)
			name := opts.Name
			if name == "" {
				name = shortID(id)
			}
			msg := fmt.Sprintf("Container %s created.", name)
			if opts.Start {
				msg = fmt.Sprintf("Container %s created and started.", name)
			}
			sessionFrom(r).SetFlash(msg)
			http.Redirect(w, r, "/containers", http.StatusSeeOther)
			return
		}
		if id != "" {
			// Created, but starting failed.
			sessionFrom(r).SetFlash(userError(err))
			http.Redirect(w, r, "/containers/"+id, http.StatusSeeOther)
			return
		}
	}
	s.log.Warn("container create failed", "image", f.Image, "user", sessionFrom(r).Username, "error", err)
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, "container_new", s.newPage(r, "Create container", "containers", s.newContainerView(r.Context(), f, userError(err))))
}

func (f containerForm) toOpts() (runtime.CreateContainerOpts, error) {
	opts := runtime.CreateContainerOpts{
		Image: f.Image, Name: f.Name,
		Pull: runtime.PullPolicy(f.Pull), Start: f.Start,
		TTY: f.TTY, Interactive: f.Interactive, AutoRemove: f.AutoRemove, NoHealthcheck: f.NoHealthcheck,
		CPUs: f.CPUs, Memory: f.Memory, Hostname: f.Hostname, User: f.User,
		DNS: fields(f.DNS), DNSSearch: fields(f.DNSSearch), DNSOptions: fields(f.DNSOptions), Ulimits: fields(f.Ulimits),
		RestartPolicy: f.Restart, WorkDir: f.WorkDir, Labels: lines(f.Labels),
		ExtraHosts: fields(f.ExtraHosts), CapAdd: fields(f.CapAdd), CapDrop: fields(f.CapDrop), Privileged: f.Privileged,
	}
	if opts.RestartPolicy == "no" {
		opts.RestartPolicy = ""
	}
	var errs []error
	cmd, err := splitArgs(f.Cmd)
	if err != nil {
		errs = append(errs, err)
	}
	opts.Cmd = cmd
	if strings.TrimSpace(f.Entrypoint) != "" {
		if opts.Entrypoint, err = splitArgs(f.Entrypoint); err != nil {
			errs = append(errs, err)
		}
	}
	if f.Network != "" {
		opts.Networks = []runtime.NetworkAttachment{{Name: f.Network, Aliases: fields(f.NetworkAliases), IP: f.NetworkIP}}
	} else if f.NetworkAliases != "" || f.NetworkIP != "" {
		errs = append(errs, fmt.Errorf("%w: network aliases and IP address need a network other than the default", runtime.ErrInvalid))
	}
	for _, l := range lines(f.ExtraNetworks) {
		n, err := parseNetworkLine(l)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(opts.Networks) == 0 {
			errs = append(errs, fmt.Errorf("%w: choose the first network before adding more networks", runtime.ErrInvalid))
			break
		}
		opts.Networks = append(opts.Networks, n)
	}
	opts.Env = lines(f.Env)
	for _, spec := range fields(f.Ports) {
		p, err := runtime.ParsePortSpec(spec)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		opts.Ports = append(opts.Ports, p)
	}
	for _, spec := range lines(f.Volumes) {
		v, err := runtime.ParseVolumeSpec(spec)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		opts.Volumes = append(opts.Volumes, v)
	}
	if err := errors.Join(errs...); err != nil {
		return opts, err
	}
	return opts, opts.Validate()
}

// parseNetworkLine parses "name [alias=a,b] [ip=x.x.x.x]".
func parseNetworkLine(l string) (runtime.NetworkAttachment, error) {
	parts := strings.Fields(l)
	n := runtime.NetworkAttachment{Name: parts[0]}
	for _, p := range parts[1:] {
		k, v, _ := strings.Cut(p, "=")
		switch k {
		case "alias", "aliases":
			n.Aliases = append(n.Aliases, fields(v)...)
		case "ip":
			n.IP = v
		default:
			return n, fmt.Errorf("%w: network line %q: use name [alias=a,b] [ip=x.x.x.x]", runtime.ErrInvalid, l)
		}
	}
	return n, nil
}

// formFromOpts fills the form from create options (the edit form).
func formFromOpts(o runtime.CreateContainerOpts) containerForm {
	f := containerForm{
		Image: o.Image, Name: o.Name, Cmd: joinArgs(o.Cmd), Env: strings.Join(o.Env, "\n"),
		Pull: string(o.Pull), Start: o.Start,
		TTY: o.TTY, Interactive: o.Interactive, AutoRemove: o.AutoRemove, NoHealthcheck: o.NoHealthcheck, Privileged: o.Privileged,
		CPUs: o.CPUs, Memory: o.Memory, Hostname: o.Hostname, User: o.User,
		DNS: strings.Join(o.DNS, ", "), DNSSearch: strings.Join(o.DNSSearch, ", "), DNSOptions: strings.Join(o.DNSOptions, " "),
		Ulimits: strings.Join(o.Ulimits, "\n"), Restart: o.RestartPolicy, Entrypoint: joinArgs(o.Entrypoint), WorkDir: o.WorkDir,
		Labels: strings.Join(o.Labels, "\n"), ExtraHosts: strings.Join(o.ExtraHosts, ", "),
		CapAdd: strings.Join(o.CapAdd, ", "), CapDrop: strings.Join(o.CapDrop, ", "),
	}
	if f.Pull == "" {
		f.Pull = string(runtime.PullMissing)
	}
	var ports, vols, nets []string
	for _, p := range o.Ports {
		ports = append(ports, p.PortSpec())
	}
	for _, v := range o.Volumes {
		vols = append(vols, v.String())
	}
	f.Ports, f.Volumes = strings.Join(ports, "\n"), strings.Join(vols, "\n")
	for i, n := range o.Networks {
		if i == 0 {
			f.Network, f.NetworkAliases, f.NetworkIP = n.Name, strings.Join(n.Aliases, ", "), n.IP
			continue
		}
		l := n.Name
		if len(n.Aliases) > 0 {
			l += " alias=" + strings.Join(n.Aliases, ",")
		}
		if n.IP != "" {
			l += " ip=" + n.IP
		}
		nets = append(nets, l)
	}
	f.ExtraNetworks = strings.Join(nets, "\n")
	return f
}

// joinArgs renders arguments as splitArgs reads them.
func joinArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a != "" && !strings.ContainsAny(a, " \t\n\r\"'\\") {
			out[i] = a
			continue
		}
		out[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a) + `"`
	}
	return strings.Join(out, " ")
}

// lines splits a textarea into trimmed, non-empty lines.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// fields splits on newlines, commas and spaces.
func fields(s string) []string {
	out := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t' })
	if len(out) == 0 {
		return nil
	}
	return out
}

// splitArgs splits a command line into arguments, honoring single and double
// quotes and backslash escapes (inside double quotes and unquoted).
func splitArgs(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inArg := false
	var quote rune
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, inArg = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inArg = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("%w: unterminated quote in command", runtime.ErrInvalid)
	}
	if escaped {
		cur.WriteRune('\\')
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}

// ---- images ----

type imagesView struct {
	Images []runtime.Image
	Err    string
	Flash  *flash
	Caps   runtime.Capabilities
	CSRF   string
}

type pullsView struct {
	Jobs       []jobs.Job
	AnyRunning bool
	Err        string // validation error for the pull form
}

type imagesPage struct {
	Images imagesView
	Pulls  pullsView
}

func (s *Server) loadImages(r *http.Request, f *flash) imagesView {
	v := imagesView{Flash: f, Caps: s.caps(), CSRF: sessionFrom(r).CSRF}
	imgs, err := s.rt.ListImages(r.Context())
	if err != nil {
		v.Err = userError(err)
	}
	v.Images = imgs
	return v
}

func (s *Server) loadPulls() pullsView {
	v := pullsView{Jobs: s.jobs.List("pull")}
	if len(v.Jobs) > 5 {
		v.Jobs = v.Jobs[:5]
	}
	for _, j := range v.Jobs {
		if !j.Done() {
			v.AnyRunning = true
		}
	}
	return v
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	data := imagesPage{Images: s.loadImages(r, nil), Pulls: s.loadPulls()}
	s.render(w, "images", s.newPage(r, "Images", "images", data))
}

func (s *Server) handleImagesPartial(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, "images_table", s.loadImages(r, nil))
}

func (s *Server) handlePullsPartial(w http.ResponseWriter, r *http.Request) {
	v := s.loadPulls()
	if !v.AnyRunning && isHTMX(r) {
		// The polling panel stops after this response; let the image
		// table reload once to show the newly pulled image.
		w.Header().Set("HX-Trigger", "images-changed")
	}
	s.renderPartial(w, "pulls_panel", v)
}

func (s *Server) handleImagePull(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSpace(r.PostFormValue("ref"))
	user := sessionFrom(r).Username
	if !s.caps().Pull {
		s.renderPullError(w, r, runtime.ErrUnsupported)
		return
	}
	if err := runtime.ValidateImageRef(ref); err != nil {
		s.renderPullError(w, r, err)
		return
	}
	s.log.Info("image pull started", "ref", ref, "user", user)
	s.jobs.Start("pull", ref, user, func(ctx context.Context, progress func(string)) error {
		err := s.rt.PullImage(ctx, ref, progress)
		if err != nil {
			s.log.Warn("image pull failed", "ref", ref, "user", user, "error", err)
			return errors.New(userError(err))
		}
		s.log.Info("image pulled", "ref", ref, "user", user)
		return nil
	})
	if !isHTMX(r) {
		http.Redirect(w, r, "/images", http.StatusSeeOther)
		return
	}
	s.renderPartial(w, "pulls_panel", s.loadPulls())
}

func (s *Server) renderPullError(w http.ResponseWriter, r *http.Request, err error) {
	if !isHTMX(r) {
		sessionFrom(r).SetFlash(userError(err))
		http.Redirect(w, r, "/images", http.StatusSeeOther)
		return
	}
	v := s.loadPulls()
	v.Err = userError(err)
	s.renderPartial(w, "pulls_panel", v)
}

func (s *Server) imageResult(w http.ResponseWriter, r *http.Request, f *flash) {
	if !isHTMX(r) {
		sessionFrom(r).SetFlash(f.Msg)
		http.Redirect(w, r, "/images", http.StatusSeeOther)
		return
	}
	s.renderPartial(w, "images_table", s.loadImages(r, f))
}

func (s *Server) handleImageTag(w http.ResponseWriter, r *http.Request) {
	source := r.PostFormValue("source")
	target := strings.TrimSpace(r.PostFormValue("target"))
	var err error
	if !s.caps().Tag {
		err = runtime.ErrUnsupported
	} else {
		err = s.rt.TagImage(r.Context(), source, target)
	}
	if err != nil {
		s.log.Warn("image tag failed", "source", source, "target", target, "error", err)
		s.imageResult(w, r, errFlash(err))
		return
	}
	s.log.Info("image tagged", "source", source, "target", target, "user", sessionFrom(r).Username)
	s.imageResult(w, r, okFlash("Tagged %s as %s.", source, target))
}

func (s *Server) handleImageDelete(w http.ResponseWriter, r *http.Request) {
	ref := r.PostFormValue("ref")
	force := r.PostFormValue("force") == "true"
	if err := s.rt.RemoveImage(r.Context(), ref, force); err != nil {
		s.log.Warn("image delete failed", "ref", ref, "error", err)
		f := errFlash(err)
		if errors.Is(err, runtime.ErrConflict) {
			f.Msg += " The image is used by a container; delete the container first, or refresh the list and delete again to remove it anyway."
		}
		s.imageResult(w, r, f)
		return
	}
	s.log.Info("image deleted", "ref", ref, "force", force, "user", sessionFrom(r).Username)
	s.imageResult(w, r, okFlash("Deleted image %s.", ref))
}

// ---- volumes ----

type volumeRow struct {
	runtime.Volume
	UsedBy []string // names of the containers mounting the volume
}

type volumesView struct {
	Volumes []volumeRow
	Err     string
	Flash   *flash
	CSRF    string
}

type volumeForm struct {
	Name, Driver, Options, Labels string
}

// AdvancedChanged reports whether the collapsed options section has input.
func (f volumeForm) AdvancedChanged() bool { return f.Options != "" || f.Labels != "" }

type volumesPage struct {
	Volumes volumesView
	Form    volumeForm
	FormErr string // error from the create form
	Drivers []string
	// PruneAnonymous: prune can be limited to anonymous volumes.
	PruneAnonymous bool
}

type volumeDetailView struct {
	Name    string
	Volume  *volumeRow
	Inspect string
	Err     string
	CSRF    string
}

// volumesSupported answers 404 if the runtime has no volume support.
func (s *Server) volumesSupported(w http.ResponseWriter, r *http.Request) bool {
	if !s.caps().Volumes {
		http.NotFound(w, r)
		return false
	}
	return true
}

// volumeUsers maps volume names to the containers that mount them. A failed
// container listing only leaves the "used by" information empty.
func (s *Server) volumeUsers(ctx context.Context) map[string][]string {
	users := map[string][]string{}
	cs, _ := s.rt.ListContainers(ctx, runtime.ListContainersOpts{All: true})
	for _, c := range cs {
		for _, m := range c.Mounts {
			users[m] = append(users[m], c.Name)
		}
	}
	return users
}

func (s *Server) loadVolumes(r *http.Request, f *flash) volumesView {
	v := volumesView{Flash: f, CSRF: sessionFrom(r).CSRF}
	vols, err := s.rt.ListVolumes(r.Context())
	if err != nil {
		v.Err = userError(err)
		return v
	}
	users := s.volumeUsers(r.Context())
	for _, vol := range vols {
		v.Volumes = append(v.Volumes, volumeRow{Volume: vol, UsedBy: users[vol.Name]})
	}
	return v
}

func (s *Server) renderVolumesPage(w http.ResponseWriter, r *http.Request, f volumeForm, formErr string) {
	caps := s.caps()
	data := volumesPage{Volumes: s.loadVolumes(r, nil), Form: f, FormErr: formErr, Drivers: caps.VolumeDrivers, PruneAnonymous: caps.PruneAnonymous}
	s.render(w, "volumes", s.newPage(r, "Volumes", "volumes", data))
}

func (s *Server) handleVolumes(w http.ResponseWriter, r *http.Request) {
	if s.volumesSupported(w, r) {
		s.renderVolumesPage(w, r, volumeForm{}, "")
	}
}

func (s *Server) handleVolumesPartial(w http.ResponseWriter, r *http.Request) {
	if s.volumesSupported(w, r) {
		s.renderPartial(w, "volumes_table", s.loadVolumes(r, nil))
	}
}

func (s *Server) handleVolumeDetail(w http.ResponseWriter, r *http.Request) {
	if !s.volumesSupported(w, r) {
		return
	}
	name := r.PathValue("name")
	v := volumeDetailView{Name: name, CSRF: sessionFrom(r).CSRF}
	for _, row := range s.loadVolumes(r, nil).Volumes {
		if row.Name == name {
			v.Volume = &row
		}
	}
	raw, err := s.rt.InspectVolume(r.Context(), name)
	if err != nil {
		v.Err = userError(err)
	} else {
		v.Inspect = prettyJSON(raw)
	}
	if v.Volume == nil && v.Inspect == "" {
		w.WriteHeader(http.StatusNotFound)
		if v.Err == "" {
			v.Err = "Volume " + name + " not found."
		}
	}
	s.render(w, "volume_detail", s.newPage(r, "Volume "+name, "volumes", v))
}

func (s *Server) handleVolumeCreate(w http.ResponseWriter, r *http.Request) {
	if !s.volumesSupported(w, r) {
		return
	}
	f := volumeForm{
		Name:    strings.TrimSpace(r.PostFormValue("name")),
		Driver:  r.PostFormValue("driver"),
		Options: r.PostFormValue("options"),
		Labels:  r.PostFormValue("labels"),
	}
	opts := runtime.CreateVolumeOpts{Name: f.Name, Driver: f.Driver, Options: lines(f.Options), Labels: lines(f.Labels)}
	name, err := s.rt.CreateVolume(r.Context(), opts)
	if err != nil {
		s.log.Warn("volume create failed", "name", f.Name, "user", sessionFrom(r).Username, "error", err)
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.renderVolumesPage(w, r, f, userError(err))
		return
	}
	s.log.Info("volume created", "name", name, "driver", opts.Driver, "user", sessionFrom(r).Username)
	if f.Name == "" {
		name = shortID(name)
	}
	sessionFrom(r).SetFlash(fmt.Sprintf("Volume %s created.", name))
	http.Redirect(w, r, "/volumes", http.StatusSeeOther)
}

// volumeResult answers a volume action: htmx gets a fresh table, other
// clients a redirect to the list.
func (s *Server) volumeResult(w http.ResponseWriter, r *http.Request, f *flash) {
	if !isHTMX(r) {
		sessionFrom(r).SetFlash(f.Msg)
		http.Redirect(w, r, "/volumes", http.StatusSeeOther)
		return
	}
	s.renderPartial(w, "volumes_table", s.loadVolumes(r, f))
}

func (s *Server) handleVolumeDelete(w http.ResponseWriter, r *http.Request) {
	if !s.volumesSupported(w, r) {
		return
	}
	name := r.PostFormValue("name")
	force := r.PostFormValue("force") == "true"
	err := s.rt.RemoveVolume(r.Context(), name, force)
	var f *flash
	if err != nil {
		s.log.Warn("volume delete failed", "name", name, "force", force, "error", err)
		f = errFlash(err)
		if errors.Is(err, runtime.ErrConflict) {
			f.Msg += " The volume is used by a container; delete the container first."
		}
	} else {
		s.log.Info("volume deleted", "name", name, "force", force, "user", sessionFrom(r).Username)
		f = okFlash("Deleted volume %s.", name)
	}
	if r.PostFormValue("view") == "detail" {
		// Back to the detail page on failure, to the list once it is gone.
		sessionFrom(r).SetFlash(f.Msg)
		to := "/volumes"
		if err != nil {
			to = "/volumes/" + url.PathEscape(name)
		}
		redirect(w, r, to)
		return
	}
	s.volumeResult(w, r, f)
}

func (s *Server) handleVolumePrune(w http.ResponseWriter, r *http.Request) {
	if !s.volumesSupported(w, r) {
		return
	}
	all := r.PostFormValue("all") == "on" || !s.caps().PruneAnonymous
	out, err := s.rt.PruneVolumes(r.Context(), all)
	if err != nil {
		s.log.Warn("volume prune failed", "all", all, "error", err)
		s.volumeResult(w, r, errFlash(err))
		return
	}
	s.log.Info("volumes pruned", "all", all, "user", sessionFrom(r).Username)
	f := okFlash("Pruned unused volumes.")
	if out != "" {
		f.Msg += " " + strings.Join(lines(out), " ")
	}
	s.volumeResult(w, r, f)
}
