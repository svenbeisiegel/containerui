package runtime

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
)

// CreateNetworkOpts describes a network to create.
type CreateNetworkOpts struct {
	Name     string   // required
	Driver   string   // optional; one of Capabilities.NetworkDrivers
	Subnet   string   // CIDR, e.g. 172.30.0.0/16
	Gateway  string   // IP inside Subnet
	IPRange  string   // CIDR inside Subnet that container IPs are taken from
	Internal bool     // no external access
	IPv6     bool     // needs Capabilities.NetworkIPv6
	Options  []string // driver options, key=value
	Labels   []string // metadata, key=value
}

// ConnectNetworkOpts are the optional settings of a container's endpoint.
type ConnectNetworkOpts struct {
	Aliases []string // extra DNS names of the container in the network
	IP      string   // fixed IPv4 address
}

// predefinedNetworks are created by the runtimes themselves and cannot be
// removed.
var predefinedNetworks = []string{"bridge", "host", "none", "podman"}

// IsPredefinedNetwork reports whether name is one of the runtime's own
// networks.
func IsPredefinedNetwork(name string) bool { return slices.Contains(predefinedNetworks, name) }

// ValidateNetworkName checks a network name (or ID).
func ValidateNetworkName(name string) error {
	if !netNameRe.MatchString(name) {
		return fmt.Errorf("%w: network name %q must match %s", ErrInvalid, name, netNameRe)
	}
	return nil
}

// Validate checks the options; drivers are the network drivers the runtime
// accepts and ipv6 whether it supports IPv6 networks.
func (o *CreateNetworkOpts) Validate(drivers []string, ipv6 bool) error {
	var errs []error
	if o.Name == "" {
		errs = append(errs, fmt.Errorf("%w: a network name is required", ErrInvalid))
	} else if err := ValidateNetworkName(o.Name); err != nil {
		errs = append(errs, err)
	}
	if o.Driver != "" && !slices.Contains(drivers, o.Driver) {
		errs = append(errs, fmt.Errorf("%w: network driver %q must be one of %s", ErrInvalid, o.Driver, strings.Join(drivers, ", ")))
	}
	var subnet *net.IPNet
	if o.Subnet != "" {
		ip, n, err := net.ParseCIDR(o.Subnet)
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: subnet %q must be CIDR notation, e.g. 172.30.0.0/16", ErrInvalid, o.Subnet))
		} else {
			subnet = n
			if ip.To4() == nil && !o.IPv6 {
				errs = append(errs, fmt.Errorf("%w: subnet %q is IPv6; enable IPv6", ErrInvalid, o.Subnet))
			}
		}
	}
	if o.Gateway != "" {
		ip := net.ParseIP(o.Gateway)
		switch {
		case ip == nil:
			errs = append(errs, fmt.Errorf("%w: gateway %q is not an IP address", ErrInvalid, o.Gateway))
		case o.Subnet == "":
			errs = append(errs, fmt.Errorf("%w: a gateway needs a subnet", ErrInvalid))
		case subnet != nil && !subnet.Contains(ip):
			errs = append(errs, fmt.Errorf("%w: gateway %s is not in subnet %s", ErrInvalid, o.Gateway, o.Subnet))
		}
	}
	if o.IPRange != "" {
		ip, r, err := net.ParseCIDR(o.IPRange)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%w: IP range %q must be CIDR notation, e.g. 172.30.5.0/24", ErrInvalid, o.IPRange))
		case o.Subnet == "":
			errs = append(errs, fmt.Errorf("%w: an IP range needs a subnet", ErrInvalid))
		case subnet != nil:
			rangeOnes, _ := r.Mask.Size()
			subnetOnes, _ := subnet.Mask.Size()
			if !subnet.Contains(ip) || rangeOnes < subnetOnes {
				errs = append(errs, fmt.Errorf("%w: IP range %s is not inside subnet %s", ErrInvalid, o.IPRange, o.Subnet))
			}
		}
	}
	if o.IPv6 && !ipv6 {
		errs = append(errs, fmt.Errorf("%w: this runtime cannot create IPv6 networks", ErrInvalid))
	}
	errs = append(errs, validateKeyValues("network", "option", o.Options), validateKeyValues("network", "label", o.Labels))
	return errors.Join(errs...)
}

// Validate checks the endpoint settings.
func (o *ConnectNetworkOpts) Validate() error {
	var errs []error
	for _, a := range o.Aliases {
		if !validDomain(a) {
			errs = append(errs, fmt.Errorf("%w: alias %q must be a host name", ErrInvalid, a))
		}
	}
	if o.IP != "" {
		if ip := net.ParseIP(o.IP); ip == nil || ip.To4() == nil {
			errs = append(errs, fmt.Errorf("%w: %q is not an IPv4 address", ErrInvalid, o.IP))
		}
	}
	return errors.Join(errs...)
}

// validateKeyValues checks key=value options or labels of an object.
func validateKeyValues(object, what string, vals []string) error {
	var errs []error
	for _, v := range vals {
		if key, _, ok := strings.Cut(v, "="); !ok || !volumeKeyRe.MatchString(key) {
			errs = append(errs, fmt.Errorf("%w: %s %s %q must be key=value", ErrInvalid, object, what, v))
		}
	}
	return errors.Join(errs...)
}
