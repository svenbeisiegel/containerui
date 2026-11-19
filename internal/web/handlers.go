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

	"github.com/svenbeisiegel/containerui/internal/runtime"
	"github.com/svenbeisiegel/containerui/internal/updates"
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
// shows its own error. Showing the dashboard makes the metrics sampler
// sample at its dashboard interval (see metricsSampler).
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

// handleDashboard shows the dashboard with the history and disk usage
// collected in the background, and measures the disk usage once more; the
// partial the page polls shows the result when it is done.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if s.metrics != nil && s.caps().Stats {
		s.metrics.refreshDisk()
	}
	s.render(w, "dashboard", s.newPage(r, "Dashboard", "dashboard", s.loadDashboard(r.Context())))
}

func (s *Server) handleDashboardPartial(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, "dashboard_body", s.loadDashboard(r.Context()))
}

// ---- containers ----

type containersView struct {
	Containers []runtime.Container
	// Updates lists the newer images a container can be updated to, by ID.
	Updates map[string][]updates.Candidate
	Err     string
	Flash   *flash
	Caps    runtime.Capabilities
	CSRF    string
}

func (s *Server) loadContainers(r *http.Request, f *flash) containersView {
	v := containersView{Flash: f, Caps: s.caps(), CSRF: sessionFrom(r).CSRF}
	cs, err := s.rt.ListContainers(r.Context(), runtime.ListContainersOpts{All: true})
	if err != nil {
		v.Err = userError(err)
	}
	v.Containers = cs
	if s.updates != nil {
		v.Updates = map[string][]updates.Candidate{}
		for _, c := range cs {
			if cands := s.updates.ForContainer(c); len(cands) > 0 {
				v.Updates[c.ID] = cands
			}
		}
	}
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
	// Project is the compose project managing the container ("" if none).
	Project string
	// Updates lists the newer images the update check found; the
	// container list's menu offers to update to them.
	Updates []updates.Candidate
	// Job is the running or recent recreate job of the container.
	Job *recreateJobView
}

func (s *Server) handleContainerDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v := containerDetailView{ID: id, Caps: s.caps()}
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
		if s.updates != nil {
			v.Updates = s.updates.ForContainer(*v.Container)
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

	// Import from inspect output (create only): the container the form was
	// filled from, or the rejected paste and why.
	Import     *containerImport
	ImportText string
	ImportErr  string
}

func (s *Server) newContainerView(ctx context.Context, f containerForm, err string) containerNewView {
	v := containerNewView{Form: f, Err: err}
	v.Images, _ = s.rt.ListImages(ctx)
	if s.caps().Networks {
		v.Networks, _ = s.rt.ListNetworks(ctx)
	}
	return v
}

// defaultContainerForm is the empty create form.
func defaultContainerForm() containerForm {
	return containerForm{Pull: string(runtime.PullMissing), Start: true, TTY: true, Interactive: true}
}

func (s *Server) handleContainerNew(w http.ResponseWriter, r *http.Request) {
	f := defaultContainerForm()
	f.Image = r.URL.Query().Get("image")
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
		s.setPullLabel(&opts, opts.Pull)
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
	// Updates is what the update check found, by image reference; Scan
	// is nil without update checks.
	Updates map[string]updates.Image
	// UpdateCount: how many listed images have an update.
	UpdateCount int
	Scan        *updates.State
	Err         string
	Flash       *flash
	Caps        runtime.Capabilities
	CSRF        string
	// Partial: rendered as an htmx partial, so it also updates the update
	// check in the page head out of band.
	Partial bool
}

type imagesPage struct {
	Images   imagesView
	Activity activityView
}

// imagePullView is the "Pull image" page.
type imagePullView struct {
	Ref      string
	Err      string
	Activity activityView
}

func (s *Server) loadImages(r *http.Request, f *flash) imagesView {
	v := imagesView{Flash: f, Caps: s.caps(), CSRF: sessionFrom(r).CSRF}
	imgs, err := s.rt.ListImages(r.Context())
	if err != nil {
		v.Err = userError(err)
	}
	v.Images = imgs
	if s.updates != nil {
		st := s.updates.State()
		v.Scan = &st
		v.Updates = map[string]updates.Image{}
		for _, img := range imgs {
			if u, ok := s.updates.Image(img.Ref()); ok && img.Tag != "<none>" {
				v.Updates[img.Ref()] = u
				if u.Available() {
					v.UpdateCount++
				}
			}
		}
	}
	return v
}

// handleImageCheckUpdates starts checking all local images right away.
// With pull set, a job runs the check and then pulls every update found
// (see pullUpdates).
func (s *Server) handleImageCheckUpdates(w http.ResponseWriter, r *http.Request) {
	var f *flash
	user := sessionFrom(r).Username
	pull := r.PostFormValue("pull") == "true" && s.caps().Pull
	switch {
	case s.updates == nil:
		f = errFlash(runtime.ErrUnsupported)
	case pull:
		run, ok := s.updates.BeginCheck()
		if !ok {
			f = &flash{Error: true, Msg: "An update check is already running."}
			break
		}
		s.log.Info("image update check started", "user", user, "pull", true)
		s.jobs.Start(pullJobKind, "check for updates and pull", user, func(ctx context.Context, progress func(string)) error {
			progress("Checking all local images for updates…")
			if err := run(ctx); err != nil {
				return fmt.Errorf("update check failed: %s", userError(err))
			}
			return s.pullUpdates(ctx, user, progress)
		})
		s.openActivity(w, r, "images", !isHTMX(r))
	case s.updates.CheckNow():
		s.log.Info("image update check started", "user", user)
	}
	if !isHTMX(r) {
		if f != nil {
			sessionFrom(r).SetFlash(f.Msg)
		}
		http.Redirect(w, r, "/images", http.StatusSeeOther)
		return
	}
	s.renderImagesTable(w, r, f)
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	data := imagesPage{Images: s.loadImages(r, nil), Activity: s.loadActivity(r, "images")}
	s.render(w, "images", s.newPage(r, "Images", "images", data))
}

// renderImagesTable answers htmx with the images table and the update
// check state of the page head.
func (s *Server) renderImagesTable(w http.ResponseWriter, r *http.Request, f *flash) {
	v := s.loadImages(r, f)
	v.Partial = true
	s.renderPartial(w, "images_table", v)
}

func (s *Server) handleImagesPartial(w http.ResponseWriter, r *http.Request) {
	s.renderImagesTable(w, r, nil)
}

func (s *Server) renderPullPage(w http.ResponseWriter, r *http.Request, ref, errMsg string) {
	v := imagePullView{Ref: ref, Err: errMsg, Activity: s.loadActivity(r, "images")}
	s.render(w, "image_pull", s.newPage(r, "Pull image", "images", v))
}

func (s *Server) handleImagePullPage(w http.ResponseWriter, r *http.Request) {
	if !s.caps().Pull {
		http.NotFound(w, r)
		return
	}
	s.renderPullPage(w, r, "", "")
}

func (s *Server) handleImagePull(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSpace(r.PostFormValue("ref"))
	user := sessionFrom(r).Username
	if !s.caps().Pull {
		http.NotFound(w, r)
		return
	}
	if err := runtime.ValidateImageRef(ref); err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.renderPullPage(w, r, ref, userError(err))
		return
	}
	s.log.Info("image pull started", "ref", ref, "user", user)
	s.jobs.Start(pullJobKind, ref, user, func(ctx context.Context, progress func(string)) error {
		return s.pullImage(ctx, ref, user, progress)
	})
	if isHTMX(r) {
		// The update buttons in the image table: open the drawer in place.
		s.openActivity(w, r, "images", false)
		w.WriteHeader(http.StatusOK)
		return
	}
	s.openActivity(w, r, "images", true)
	http.Redirect(w, r, "/images", http.StatusSeeOther)
}

// pullImage pulls ref and checks it for updates again.
func (s *Server) pullImage(ctx context.Context, ref, user string, progress func(string)) error {
	err := s.rt.PullImage(ctx, ref, progress)
	if err != nil {
		s.log.Warn("image pull failed", "ref", ref, "user", user, "error", err)
		return errors.New(userError(err))
	}
	s.log.Info("image pulled", "ref", ref, "user", user)
	if s.updates != nil {
		s.updates.Refresh(ctx, ref)
	}
	return nil
}

// pullUpdates pulls, one after the other, what the update check offers
// in the image table: the newer image of a rebuilt tag and the newest
// newer version of a repository. A failed pull does not stop the others.
func (s *Server) pullUpdates(ctx context.Context, user string, progress func(string)) error {
	type pull struct{ ref, why string }
	var pulls []pull
	seen := map[string]bool{}
	add := func(ref, why string) {
		if !seen[ref] {
			seen[ref] = true
			pulls = append(pulls, pull{ref, why})
		}
	}
	for _, u := range s.updates.Updates() {
		if u.DigestChanged() {
			add(u.Ref, "rebuilt in the registry")
		}
		if tags := u.NewerTags(); len(tags) > 0 {
			add(u.Repo()+":"+tags[0].Name, "newest "+tags[0].Kind+" version of "+u.Ref)
		}
	}
	if len(pulls) == 0 {
		progress("No updates found.")
		return nil
	}
	var failed []string
	for _, p := range pulls {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		progress(fmt.Sprintf("Pulling %s (%s)…", p.ref, p.why))
		if err := s.pullImage(ctx, p.ref, user, progress); err != nil {
			progress(fmt.Sprintf("Pulling %s failed: %v", p.ref, err))
			failed = append(failed, p.ref)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d pulls failed: %s", len(failed), len(pulls), strings.Join(failed, ", "))
	}
	progress("Pulled " + plural(len(pulls), "image") + ".")
	return nil
}

// imageResult records an image action in the Images drawer and answers
// with a fresh table (htmx) or a redirect to the list.
func (s *Server) imageResult(w http.ResponseWriter, r *http.Request, target, msg, errMsg string, out []string) {
	s.recordActivity(w, r, imageJobKind, target, msg, errMsg, out, !isHTMX(r))
	if !isHTMX(r) {
		http.Redirect(w, r, "/images", http.StatusSeeOther)
		return
	}
	s.renderImagesTable(w, r, nil)
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
		s.imageResult(w, r, "tag "+source+" → "+target, "", userError(err), nil)
		return
	}
	s.log.Info("image tagged", "source", source, "target", target, "user", sessionFrom(r).Username)
	s.imageResult(w, r, "tag "+source+" → "+target, fmt.Sprintf("Tagged %s as %s.", source, target), "", nil)
}

func (s *Server) handleImageDelete(w http.ResponseWriter, r *http.Request) {
	ref := r.PostFormValue("ref")
	force := r.PostFormValue("force") == "true"
	if err := s.rt.RemoveImage(r.Context(), ref, force); err != nil {
		s.log.Warn("image delete failed", "ref", ref, "error", err)
		msg := userError(err)
		if errors.Is(err, runtime.ErrConflict) {
			msg += " The image is used by a container; delete the container first, or refresh the list and delete again to remove it anyway."
		}
		s.imageResult(w, r, "delete "+ref, "", msg, nil)
		return
	}
	s.log.Info("image deleted", "ref", ref, "force", force, "user", sessionFrom(r).Username)
	if s.updates != nil {
		s.updates.Refresh(r.Context(), ref)
	}
	s.imageResult(w, r, "delete "+ref, fmt.Sprintf("Deleted image %s.", ref), "", nil)
}

// handleImagePrune removes every image no container uses
// (image prune --all).
func (s *Server) handleImagePrune(w http.ResponseWriter, r *http.Request) {
	const target = "prune --all"
	out, err := s.rt.PruneImages(r.Context())
	if err != nil {
		s.log.Warn("image prune failed", "error", err)
		s.imageResult(w, r, target, "", userError(err), nil)
		return
	}
	s.log.Info("images pruned", "user", sessionFrom(r).Username)
	s.imageResult(w, r, target, "Pruned unused images.", "", lines(out))
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
	Volumes  volumesView
	Activity activityView
	// PruneAnonymous: prune can be limited to anonymous volumes.
	PruneAnonymous bool
}

// volumeNewView is the "Create volume" page.
type volumeNewView struct {
	Form     volumeForm
	Err      string // error from the create form
	Drivers  []string
	Activity activityView
}

type volumeDetailView struct {
	Name     string
	Volume   *volumeRow
	Inspect  string
	Err      string
	CSRF     string
	Activity activityView
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

func (s *Server) handleVolumes(w http.ResponseWriter, r *http.Request) {
	if !s.volumesSupported(w, r) {
		return
	}
	data := volumesPage{Volumes: s.loadVolumes(r, nil), Activity: s.loadActivity(r, "volumes"), PruneAnonymous: s.caps().PruneAnonymous}
	s.render(w, "volumes", s.newPage(r, "Volumes", "volumes", data))
}

func (s *Server) renderVolumeNew(w http.ResponseWriter, r *http.Request, f volumeForm, formErr string) {
	v := volumeNewView{Form: f, Err: formErr, Drivers: s.caps().VolumeDrivers, Activity: s.loadActivity(r, "volumes")}
	s.render(w, "volume_new", s.newPage(r, "Create volume", "volumes", v))
}

func (s *Server) handleVolumeNew(w http.ResponseWriter, r *http.Request) {
	if s.volumesSupported(w, r) {
		s.renderVolumeNew(w, r, volumeForm{}, "")
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
	v := volumeDetailView{Name: name, CSRF: sessionFrom(r).CSRF, Activity: s.loadActivity(r, "volumes")}
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
		s.renderVolumeNew(w, r, f, userError(err))
		return
	}
	s.log.Info("volume created", "name", name, "driver", opts.Driver, "user", sessionFrom(r).Username)
	if f.Name == "" {
		name = shortID(name)
	}
	s.recordActivity(w, r, volumeJobKind, "create "+name, fmt.Sprintf("Volume %s created.", name), "", nil, true)
	redirect(w, r, "/volumes")
}

// volumeResult records a volume action in the Volumes drawer and answers
// with a fresh table (htmx) or a redirect to the list.
func (s *Server) volumeResult(w http.ResponseWriter, r *http.Request, target, msg, errMsg string, out []string) {
	s.recordActivity(w, r, volumeJobKind, target, msg, errMsg, out, !isHTMX(r))
	if !isHTMX(r) {
		http.Redirect(w, r, "/volumes", http.StatusSeeOther)
		return
	}
	s.renderPartial(w, "volumes_table", s.loadVolumes(r, nil))
}

func (s *Server) handleVolumeDelete(w http.ResponseWriter, r *http.Request) {
	if !s.volumesSupported(w, r) {
		return
	}
	name := r.PostFormValue("name")
	force := r.PostFormValue("force") == "true"
	err := s.rt.RemoveVolume(r.Context(), name, force)
	var msg, errMsg string
	if err != nil {
		s.log.Warn("volume delete failed", "name", name, "force", force, "error", err)
		errMsg = userError(err)
		if errors.Is(err, runtime.ErrConflict) {
			errMsg += " The volume is used by a container; delete the container first."
		}
	} else {
		s.log.Info("volume deleted", "name", name, "force", force, "user", sessionFrom(r).Username)
		msg = fmt.Sprintf("Deleted volume %s.", name)
	}
	if r.PostFormValue("view") == "detail" {
		// Back to the detail page on failure, to the list once it is gone.
		s.recordActivity(w, r, volumeJobKind, "delete "+name, msg, errMsg, nil, true)
		to := "/volumes"
		if err != nil {
			to = "/volumes/" + url.PathEscape(name)
		}
		redirect(w, r, to)
		return
	}
	s.volumeResult(w, r, "delete "+name, msg, errMsg, nil)
}

func (s *Server) handleVolumePrune(w http.ResponseWriter, r *http.Request) {
	if !s.volumesSupported(w, r) {
		return
	}
	all := r.PostFormValue("all") == "on" || !s.caps().PruneAnonymous
	target := "prune"
	if all && s.caps().PruneAnonymous {
		target += " --all"
	}
	out, err := s.rt.PruneVolumes(r.Context(), all)
	if err != nil {
		s.log.Warn("volume prune failed", "all", all, "error", err)
		s.volumeResult(w, r, target, "", userError(err), nil)
		return
	}
	s.log.Info("volumes pruned", "all", all, "user", sessionFrom(r).Username)
	s.volumeResult(w, r, target, "Pruned unused volumes.", "", lines(out))
}
