package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/svenbeisiegel/containerui/internal/runtime"
)

// ---- networks ----

type networkRow struct {
	runtime.Network
	UsedBy     []string // names of the containers attached to the network
	Predefined bool     // created by the runtime itself; cannot be deleted
}

type networksView struct {
	Networks []networkRow
	Err      string
	Flash    *flash
	CSRF     string
}

type networkForm struct {
	Name, Driver, Subnet, Gateway, IPRange, Options, Labels string
	Internal, IPv6                                          bool
}

// AdvancedChanged reports whether the collapsed advanced section has input.
func (f networkForm) AdvancedChanged() bool {
	return f.Subnet != "" || f.Gateway != "" || f.IPRange != "" || f.Internal || f.IPv6 || f.Options != "" || f.Labels != ""
}

type networksPage struct {
	Networks networksView
	Activity activityView
}

// networkNewView is the "Create network" page.
type networkNewView struct {
	Form     networkForm
	Err      string // error from the create form
	Drivers  []string
	IPv6     bool // the runtime can create IPv6 networks
	Activity activityView
}

// networkContainer is a container attached to a network.
type networkContainer struct {
	ID, Name, IPv4, IPv6 string
	Aliases              []string
}

// networkSubnet is one IPAM entry of a network.
type networkSubnet struct{ Subnet, Gateway, IPRange string }

// networkInfo is what the detail page shows from the inspect output.
type networkInfo struct {
	Subnets    []networkSubnet
	Labels     map[string]string
	Options    map[string]string
	Containers []networkContainer
}

type networkDetailView struct {
	Name      string
	Network   *networkRow
	Info      networkInfo
	Inspect   string
	Err       string
	CSRF      string
	Available []runtime.Container // containers not attached, for "Connect"
	Activity  activityView
}

// networksSupported answers 404 if the runtime has no network support.
func (s *Server) networksSupported(w http.ResponseWriter, r *http.Request) bool {
	if !s.caps().Networks {
		http.NotFound(w, r)
		return false
	}
	return true
}

// networkUsers maps network names to the containers attached to them. A
// failed container listing only leaves the "used by" information empty.
func (s *Server) networkUsers(ctx context.Context) ([]runtime.Container, map[string][]string) {
	users := map[string][]string{}
	cs, _ := s.rt.ListContainers(ctx, runtime.ListContainersOpts{All: true})
	for _, c := range cs {
		for _, n := range c.Networks {
			users[n] = append(users[n], c.Name)
		}
	}
	return cs, users
}

func (s *Server) loadNetworks(r *http.Request, f *flash) networksView {
	v := networksView{Flash: f, CSRF: sessionFrom(r).CSRF}
	nets, err := s.rt.ListNetworks(r.Context())
	if err != nil {
		v.Err = userError(err)
		return v
	}
	_, users := s.networkUsers(r.Context())
	for _, n := range nets {
		v.Networks = append(v.Networks, networkRow{Network: n, UsedBy: users[n.Name], Predefined: runtime.IsPredefinedNetwork(n.Name)})
	}
	sort.SliceStable(v.Networks, func(i, j int) bool {
		// The runtime's own networks first, then by name.
		if a, b := v.Networks[i].Predefined, v.Networks[j].Predefined; a != b {
			return a
		}
		return v.Networks[i].Name < v.Networks[j].Name
	})
	return v
}

func (s *Server) handleNetworks(w http.ResponseWriter, r *http.Request) {
	if !s.networksSupported(w, r) {
		return
	}
	data := networksPage{Networks: s.loadNetworks(r, nil), Activity: s.loadActivity(r, "networks")}
	s.render(w, "networks", s.newPage(r, "Networks", "networks", data))
}

func (s *Server) renderNetworkNew(w http.ResponseWriter, r *http.Request, f networkForm, formErr string) {
	caps := s.caps()
	v := networkNewView{Form: f, Err: formErr, Drivers: caps.NetworkDrivers, IPv6: caps.NetworkIPv6, Activity: s.loadActivity(r, "networks")}
	s.render(w, "network_new", s.newPage(r, "Create network", "networks", v))
}

func (s *Server) handleNetworkNew(w http.ResponseWriter, r *http.Request) {
	if s.networksSupported(w, r) {
		s.renderNetworkNew(w, r, networkForm{}, "")
	}
}

func (s *Server) handleNetworksPartial(w http.ResponseWriter, r *http.Request) {
	if s.networksSupported(w, r) {
		s.renderPartial(w, "networks_table", s.loadNetworks(r, nil))
	}
}

func (s *Server) handleNetworkCreate(w http.ResponseWriter, r *http.Request) {
	if !s.networksSupported(w, r) {
		return
	}
	f := networkForm{
		Name:     strings.TrimSpace(r.PostFormValue("name")),
		Driver:   r.PostFormValue("driver"),
		Subnet:   strings.TrimSpace(r.PostFormValue("subnet")),
		Gateway:  strings.TrimSpace(r.PostFormValue("gateway")),
		IPRange:  strings.TrimSpace(r.PostFormValue("ip_range")),
		Internal: r.PostFormValue("internal") == "on",
		IPv6:     r.PostFormValue("ipv6") == "on",
		Options:  r.PostFormValue("options"),
		Labels:   r.PostFormValue("labels"),
	}
	opts := runtime.CreateNetworkOpts{Name: f.Name, Driver: f.Driver, Subnet: f.Subnet, Gateway: f.Gateway, IPRange: f.IPRange,
		Internal: f.Internal, IPv6: f.IPv6, Options: lines(f.Options), Labels: lines(f.Labels)}
	if _, err := s.rt.CreateNetwork(r.Context(), opts); err != nil {
		s.log.Warn("network create failed", "name", f.Name, "user", sessionFrom(r).Username, "error", err)
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.renderNetworkNew(w, r, f, userError(err))
		return
	}
	s.log.Info("network created", "name", f.Name, "driver", f.Driver, "subnet", f.Subnet, "user", sessionFrom(r).Username)
	s.recordActivity(w, r, networkJobKind, "create "+f.Name, fmt.Sprintf("Network %s created.", f.Name), "", nil, true)
	redirect(w, r, "/networks")
}

// networkResult records a network action in the Networks drawer and answers
// with a fresh table (htmx) or a redirect to the list.
func (s *Server) networkResult(w http.ResponseWriter, r *http.Request, target, msg, errMsg string, out []string) {
	s.recordActivity(w, r, networkJobKind, target, msg, errMsg, out, !isHTMX(r))
	if !isHTMX(r) {
		http.Redirect(w, r, "/networks", http.StatusSeeOther)
		return
	}
	s.renderPartial(w, "networks_table", s.loadNetworks(r, nil))
}

func (s *Server) handleNetworkDelete(w http.ResponseWriter, r *http.Request) {
	if !s.networksSupported(w, r) {
		return
	}
	name := r.PostFormValue("name")
	err := s.rt.RemoveNetwork(r.Context(), name)
	var msg, errMsg string
	if err != nil {
		s.log.Warn("network delete failed", "name", name, "error", err)
		errMsg = userError(err)
		if errors.Is(err, runtime.ErrConflict) && !runtime.IsPredefinedNetwork(name) {
			errMsg += " Containers are still connected; disconnect them first."
		}
	} else {
		s.log.Info("network deleted", "name", name, "user", sessionFrom(r).Username)
		msg = fmt.Sprintf("Deleted network %s.", name)
	}
	if r.PostFormValue("view") == "detail" {
		// Back to the detail page on failure, to the list once it is gone.
		s.recordActivity(w, r, networkJobKind, "delete "+name, msg, errMsg, nil, true)
		to := "/networks"
		if err != nil {
			to = "/networks/" + url.PathEscape(name)
		}
		redirect(w, r, to)
		return
	}
	s.networkResult(w, r, "delete "+name, msg, errMsg, nil)
}

func (s *Server) handleNetworkPrune(w http.ResponseWriter, r *http.Request) {
	if !s.networksSupported(w, r) {
		return
	}
	out, err := s.rt.PruneNetworks(r.Context())
	if err != nil {
		s.log.Warn("network prune failed", "error", err)
		s.networkResult(w, r, "prune", "", userError(err), nil)
		return
	}
	s.log.Info("networks pruned", "user", sessionFrom(r).Username)
	msg := "Pruned unused networks."
	if out == "" {
		msg = "No unused networks to prune."
	}
	s.networkResult(w, r, "prune", msg, "", lines(out))
}

func (s *Server) handleNetworkDetail(w http.ResponseWriter, r *http.Request) {
	if !s.networksSupported(w, r) {
		return
	}
	name := r.PathValue("name")
	v := networkDetailView{Name: name, CSRF: sessionFrom(r).CSRF, Activity: s.loadActivity(r, "networks")}
	for _, row := range s.loadNetworks(r, nil).Networks {
		if row.Name == name || row.ID == name {
			v.Network = &row
		}
	}
	raw, err := s.rt.InspectNetwork(r.Context(), name)
	if err != nil {
		v.Err = userError(err)
	} else {
		v.Inspect = prettyJSON(raw)
		v.Info = parseNetworkInspect(raw)
	}
	if v.Network == nil && v.Inspect == "" {
		w.WriteHeader(http.StatusNotFound)
		if v.Err == "" {
			v.Err = "Network " + name + " not found."
		}
		s.render(w, "network_detail", s.newPage(r, "Network "+name, "networks", v))
		return
	}
	if v.Network != nil {
		v.Name = v.Network.Name
	}
	// Prefer the inspect output; fall back to the container list.
	cs, users := s.networkUsers(r.Context())
	if len(v.Info.Containers) == 0 {
		for _, c := range cs {
			if slices.Contains(c.Networks, v.Name) {
				v.Info.Containers = append(v.Info.Containers, networkContainer{ID: c.ID, Name: c.Name})
			}
		}
	}
	attached := map[string]bool{}
	for _, c := range v.Info.Containers {
		attached[c.ID], attached[c.Name] = true, true
	}
	for _, c := range cs {
		if !attached[c.ID] && !attached[c.Name] && !slices.Contains(users[v.Name], c.Name) {
			v.Available = append(v.Available, c)
		}
	}
	sort.Slice(v.Available, func(i, j int) bool { return v.Available[i].Name < v.Available[j].Name })
	s.render(w, "network_detail", s.newPage(r, "Network "+v.Name, "networks", v))
}

// parseNetworkInspect reads docker/wslc style ("IPAM", "Containers") and
// podman style ("subnets", "containers") network inspect output.
func parseNetworkInspect(raw json.RawMessage) networkInfo {
	var doc struct {
		IPAM struct {
			Config []struct{ Subnet, Gateway, IPRange string }
		}
		Subnets []struct {
			Subnet  string `json:"subnet"`
			Gateway string `json:"gateway"`
		} `json:"subnets"`
		Labels     map[string]string
		Options    map[string]string
		Containers map[string]struct {
			Name        string
			IPv4Address string
			IPv6Address string
			Aliases     []string
			// podman
			Interfaces map[string]struct {
				Subnets []struct {
					IPNet string `json:"ipnet"`
				} `json:"subnets"`
			} `json:"interfaces"`
		}
	}
	var info networkInfo
	if err := json.Unmarshal(raw, &doc); err != nil {
		return info
	}
	for _, c := range doc.IPAM.Config {
		info.Subnets = append(info.Subnets, networkSubnet{Subnet: c.Subnet, Gateway: c.Gateway, IPRange: c.IPRange})
	}
	for _, sn := range doc.Subnets {
		info.Subnets = append(info.Subnets, networkSubnet{Subnet: sn.Subnet, Gateway: sn.Gateway})
	}
	info.Labels, info.Options = doc.Labels, doc.Options
	for id, c := range doc.Containers {
		nc := networkContainer{ID: id, Name: c.Name, IPv4: c.IPv4Address, IPv6: c.IPv6Address, Aliases: c.Aliases}
		if nc.IPv4 == "" {
			for _, itf := range c.Interfaces {
				for _, sn := range itf.Subnets {
					nc.IPv4 = sn.IPNet
				}
			}
		}
		info.Containers = append(info.Containers, nc)
	}
	sort.Slice(info.Containers, func(i, j int) bool { return info.Containers[i].Name < info.Containers[j].Name })
	return info
}

// handleNetworkConnect attaches a container to the network.
func (s *Server) handleNetworkConnect(w http.ResponseWriter, r *http.Request) {
	if !s.networksSupported(w, r) {
		return
	}
	network := r.PathValue("name")
	container := r.PostFormValue("container")
	opts := runtime.ConnectNetworkOpts{Aliases: fields(r.PostFormValue("aliases")), IP: strings.TrimSpace(r.PostFormValue("ip"))}
	user := sessionFrom(r).Username
	target := network + ": connect " + container
	if container == "" {
		sessionFrom(r).SetFlash("Choose a container to connect.")
	} else if err := s.rt.ConnectNetwork(r.Context(), network, container, opts); err != nil {
		s.log.Warn("network connect failed", "network", network, "container", container, "user", user, "error", err)
		s.recordActivity(w, r, networkJobKind, target, "", userError(err), nil, true)
	} else {
		s.log.Info("network connected", "network", network, "container", container, "user", user)
		s.recordActivity(w, r, networkJobKind, target, fmt.Sprintf("Connected %s to network %s.", container, network), "", nil, true)
	}
	redirect(w, r, "/networks/"+url.PathEscape(network))
}

// handleNetworkDisconnect detaches a container from the network.
func (s *Server) handleNetworkDisconnect(w http.ResponseWriter, r *http.Request) {
	if !s.networksSupported(w, r) {
		return
	}
	network := r.PathValue("name")
	container := r.PostFormValue("container")
	user := sessionFrom(r).Username
	target := network + ": disconnect " + container
	if err := s.rt.DisconnectNetwork(r.Context(), network, container); err != nil {
		s.log.Warn("network disconnect failed", "network", network, "container", container, "user", user, "error", err)
		s.recordActivity(w, r, networkJobKind, target, "", userError(err), nil, true)
	} else {
		s.log.Info("network disconnected", "network", network, "container", container, "user", user)
		s.recordActivity(w, r, networkJobKind, target, fmt.Sprintf("Disconnected %s from network %s.", container, network), "", nil, true)
	}
	redirect(w, r, "/networks/"+url.PathEscape(network))
}
