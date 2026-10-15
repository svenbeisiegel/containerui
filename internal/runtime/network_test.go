package runtime

import (
	"errors"
	"strings"
	"testing"
)

func TestCreateNetworkOptsValidate(t *testing.T) {
	drivers := []string{"bridge", "macvlan"}
	valid := []CreateNetworkOpts{
		{Name: "backend"},
		{Name: "backend", Driver: "macvlan", Subnet: "172.30.0.0/16", Gateway: "172.30.0.1", IPRange: "172.30.5.0/24", Internal: true,
			Options: []string{"com.docker.network.driver.mtu=1400"}, Labels: []string{"team=platform", "empty="}},
		{Name: "v6", Subnet: "fd00:1::/64", IPv6: true},
	}
	for _, o := range valid {
		if err := o.Validate(drivers, true); err != nil {
			t.Errorf("Validate(%+v) = %v", o, err)
		}
	}
	for _, tc := range []struct {
		o    CreateNetworkOpts
		ipv6 bool
		want string
	}{
		{CreateNetworkOpts{}, true, "a network name is required"},
		{CreateNetworkOpts{Name: "-x"}, true, "network name"},
		{CreateNetworkOpts{Name: "n", Driver: "overlay"}, true, "network driver"},
		{CreateNetworkOpts{Name: "n", Subnet: "172.30.0.0"}, true, "CIDR"},
		{CreateNetworkOpts{Name: "n", Gateway: "172.30.0.1"}, true, "a gateway needs a subnet"},
		{CreateNetworkOpts{Name: "n", Subnet: "172.30.0.0/16", Gateway: "10.0.0.1"}, true, "is not in subnet"},
		{CreateNetworkOpts{Name: "n", Subnet: "172.30.0.0/16", Gateway: "nope"}, true, "not an IP address"},
		{CreateNetworkOpts{Name: "n", IPRange: "172.30.5.0/24"}, true, "an IP range needs a subnet"},
		{CreateNetworkOpts{Name: "n", Subnet: "172.30.0.0/16", IPRange: "172.31.0.0/24"}, true, "not inside subnet"},
		{CreateNetworkOpts{Name: "n", Subnet: "172.30.0.0/24", IPRange: "172.30.0.0/16"}, true, "not inside subnet"},
		{CreateNetworkOpts{Name: "n", IPv6: true}, false, "cannot create IPv6"},
		{CreateNetworkOpts{Name: "n", Subnet: "fd00::/64"}, true, "enable IPv6"},
		{CreateNetworkOpts{Name: "n", Options: []string{"novalue"}}, true, "network option"},
		{CreateNetworkOpts{Name: "n", Labels: []string{"=x"}}, true, "network label"},
	} {
		err := tc.o.Validate(drivers, tc.ipv6)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%+v) = %v, want %q", tc.o, err, tc.want)
		}
	}
}

func TestConnectNetworkOptsValidate(t *testing.T) {
	if err := (&ConnectNetworkOpts{Aliases: []string{"db", "db.internal"}, IP: "172.30.0.5"}).Validate(); err != nil {
		t.Error(err)
	}
	for _, o := range []ConnectNetworkOpts{{Aliases: []string{"-db"}}, {Aliases: []string{"a b"}}, {IP: "fd00::1"}, {IP: "x"}} {
		if err := o.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate(%+v) = %v", o, err)
		}
	}
	if !IsPredefinedNetwork("bridge") || IsPredefinedNetwork("backend") {
		t.Error("IsPredefinedNetwork")
	}
}
