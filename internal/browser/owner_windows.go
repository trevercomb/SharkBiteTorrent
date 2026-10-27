//go:build windows

package browser

import (
	"sharkbite/internal/socks"

	"encoding/binary"
	"net"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The browser window's proxy has no password (Chromium can't send one), so
// it checks who's connecting instead: only processes started by this app
// (WebView2's msedgewebview2.exe processes are its children and
// grandchildren) may send traffic through the VPN with it.

var (
	iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
)

const tcpTableOwnerPIDAll = 5

// GuardProxy sets up the browser window's proxy s and the window's options
// o so that only the window can use the proxy: Chromium (WebView2) can't log
// in to a SOCKS proxy, so the proxy has no password and checks who's
// connecting instead (on Linux, the same check, for WebKitGTK).
func GuardProxy(s *socks.Server, o *Options) {
	s.NoAuth, s.Allow = true, FromThisApp
}

// FromThisApp reports whether the other end of c, a connection accepted on
// 127.0.0.1, belongs to this process or one it started (directly or not).
func FromThisApp(c net.Conn) bool {
	local, ok1 := c.LocalAddr().(*net.TCPAddr)
	remote, ok2 := c.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 || !remote.IP.IsLoopback() {
		return false
	}
	pid, ok := connOwner(uint16(remote.Port), uint16(local.Port))
	return ok && descendsFromUs(pid)
}

// connOwner finds the process that owns the IPv4 loopback connection from
// port from to port to.
func connOwner(from, to uint16) (uint32, bool) {
	var size uint32
	procGetExtendedTcpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, windows.AF_INET, tcpTableOwnerPIDAll, 0)
	for range 3 {
		buf := make([]byte, size+4096)
		size = uint32(len(buf))
		r, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0,
			windows.AF_INET, tcpTableOwnerPIDAll, 0)
		if r == uintptr(windows.ERROR_INSUFFICIENT_BUFFER) {
			continue
		}
		if r != 0 {
			return 0, false
		}
		// MIB_TCPTABLE_OWNER_PID: count, then rows of state, local addr,
		// local port, remote addr, remote port, pid (all 32-bit; ports in
		// network order in the low 16 bits).
		n := binary.LittleEndian.Uint32(buf)
		const row = 24
		for i := range n {
			o := 4 + int(i)*row
			if o+row > len(buf) {
				break
			}
			lport := swap16(binary.LittleEndian.Uint32(buf[o+8:]))
			rport := swap16(binary.LittleEndian.Uint32(buf[o+16:]))
			if lport == from && rport == to {
				return binary.LittleEndian.Uint32(buf[o+20:]), true
			}
		}
		return 0, false
	}
	return 0, false
}

func swap16(v uint32) uint16 { return uint16(v>>8&0xff | v<<8&0xff00) }

// descendsFromUs walks up pid's parents looking for this process.
func descendsFromUs(pid uint32) bool {
	me := uint32(os.Getpid())
	if pid == me {
		return true
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(snap)
	parent := map[uint32]uint32{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		parent[e.ProcessID] = e.ParentProcessID
	}
	for range 8 { // WebView2's processes are a few levels down at most
		p, ok := parent[pid]
		if !ok || p == 0 || p == pid {
			return false
		}
		if p == me {
			return true
		}
		pid = p
	}
	return false
}
