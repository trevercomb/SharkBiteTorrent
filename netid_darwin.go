//go:build darwin

package main

import (
	"net"
	"strings"

	"sharkbite/internal/systool"
)

// defaultRoutes reads "netstat -rn -f inet": "default  192.168.1.1  UGScg  en0".
func defaultRoutes() []defaultRoute {
	out, err := systool.Command("netstat", "-rn", "-f", "inet").Output()
	if err != nil {
		return nil
	}
	var rs []defaultRoute
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[0] == "default" && net.ParseIP(f[1]) != nil {
			if local := ifaceIPv4(f[len(f)-1]); local != "" {
				rs = append(rs, defaultRoute{localIP: local, gateway: f[1]})
			}
		}
	}
	return rs
}

func ifaceIPv4(name string) string {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return ""
}

// gatewayMAC reads "arp -n 192.168.1.1": "? (192.168.1.1) at aa:bb:cc:dd:ee:ff on en0 …".
// macOS 15+ hides the ARP table from apps without Local Network access
// ("-- no entry", while Terminal sees it), and the app never gets asked for
// that access: it sends nothing to the home network itself. So when arp
// finds nothing, the router's MAC comes from the network configuration,
// which macOS records for each network service and doesn't hide.
func gatewayMAC(localIP, gateway string) string {
	if out, err := systool.Command("arp", "-n", gateway).Output(); err == nil {
		f := strings.Fields(string(out))
		for i := range f {
			if f[i] == "at" && i+1 < len(f) {
				return f[i+1]
			}
		}
	}
	return scutilRouterMAC(localIP, gateway)
}

// scutilRouterMAC finds the network service with this address and router
// in the dynamic store (State:/Network/Service/<id>/IPv4) and returns the
// router's MAC recorded there.
func scutilRouterMAC(localIP, gateway string) string {
	list := systool.Command("scutil")
	list.Stdin = strings.NewReader("list State:/Network/Service/[^/]+/IPv4\n")
	out, err := list.Output()
	if err != nil {
		return ""
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		// "  subKey [0] = State:/Network/Service/<id>/IPv4"
		_, key, ok := strings.Cut(line, " = ")
		if !ok || !strings.HasPrefix(key, "State:/Network/Service/") {
			continue
		}
		if n++; n > 16 {
			break
		}
		show := systool.Command("scutil")
		show.Stdin = strings.NewReader("show " + strings.TrimSpace(key) + "\n")
		if out, err := show.Output(); err == nil {
			if mac := routerMACFromScutil(string(out), localIP, gateway); mac != "" {
				return mac
			}
		}
	}
	return ""
}

// routerMACFromScutil reads one service's IPv4 dictionary as scutil shows it:
//
//	ARPResolvedHardwareAddress : 34:bc:5e:9d:ea:6d
//	ARPResolvedIPAddress : 192.168.4.1
//	Addresses : <array> {
//	  0 : 192.168.4.54
//	}
//	NetworkSignature : IPv4.Router=192.168.4.1;IPv4.RouterHardwareAddress=34:bc:5e:9d:ea:6d
//	Router : 192.168.4.1
//
// It returns the router's MAC only if the service has localIP and gateway
// as its router.
func routerMACFromScutil(out, localIP, gateway string) string {
	fields := map[string]string{}
	hasAddr, inAddrs := false, false
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " : ")
		switch {
		case inAddrs && strings.TrimSpace(line) == "}":
			inAddrs = false
		case inAddrs && ok:
			hasAddr = hasAddr || v == localIP
		case ok && k == "Addresses":
			inAddrs = true
		case ok:
			fields[k] = v
		}
	}
	if !hasAddr || fields["Router"] != gateway {
		return ""
	}
	if fields["ARPResolvedIPAddress"] == gateway && fields["ARPResolvedHardwareAddress"] != "" {
		return fields["ARPResolvedHardwareAddress"]
	}
	sig := map[string]string{}
	for _, part := range strings.Split(fields["NetworkSignature"], ";") {
		if k, v, ok := strings.Cut(part, "="); ok {
			sig[k] = v
		}
	}
	if sig["IPv4.Router"] == gateway {
		return sig["IPv4.RouterHardwareAddress"]
	}
	return ""
}
