//go:build darwin

package main

import "testing"

func TestRouterMACFromScutil(t *testing.T) {
	const show = `<dictionary> {
  ARPResolvedHardwareAddress : 34:bc:5e:9d:ea:6d
  ARPResolvedIPAddress : 192.168.4.1
  AdditionalRoutes : <array> {
    0 : <dictionary> {
      DestinationAddress : 192.168.4.54
      SubnetMask : 255.255.255.255
    }
  }
  Addresses : <array> {
    0 : 192.168.4.54
  }
  ConfirmedInterfaceName : en0
  InterfaceName : en0
  NetworkSignature : IPv4.Router=192.168.4.1;IPv4.RouterHardwareAddress=34:bc:5e:9d:ea:6d
  Router : 192.168.4.1
  SubnetMasks : <array> {
    0 : 255.255.252.0
  }
}
`
	const sigOnly = `<dictionary> {
  Addresses : <array> {
    0 : 10.0.0.5
  }
  NetworkSignature : IPv4.Router=10.0.0.1;IPv4.RouterHardwareAddress=aa:bb:cc:dd:ee:ff
  Router : 10.0.0.1
}
`
	for _, c := range []struct {
		name, out, local, gw, want string
	}{
		{"ARP entry", show, "192.168.4.54", "192.168.4.1", "34:bc:5e:9d:ea:6d"},
		{"signature only", sigOnly, "10.0.0.5", "10.0.0.1", "aa:bb:cc:dd:ee:ff"},
		{"other router", show, "192.168.4.54", "192.168.4.2", ""},
		{"other address", show, "192.168.4.55", "192.168.4.1", ""},
		{"route address isn't the service's", show, "255.255.255.255", "192.168.4.1", ""},
		{"empty", "", "192.168.4.54", "192.168.4.1", ""},
	} {
		if got := routerMACFromScutil(c.out, c.local, c.gw); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
