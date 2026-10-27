//go:build linux

package browser

import (
	"sharkbite/internal/socks"

	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The browser window's proxy has no password, so it checks who's connecting
// instead: only processes started by this app (the window's process, and
// WebKitGTK's network process under it) may send traffic through the VPN
// with it. Linux: the connection's socket is found in /proc/net/tcp, its
// owner by the socket in a process's fd table, then up the parent chain.

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
	// The client's end: local address = its port, remote = ours.
	inode := socketInode(remote.Port, local.Port)
	if inode == "" {
		return false
	}
	pid := ownerOf(inode)
	return pid > 0 && descendsFrom(pid, os.Getpid())
}

// socketInode finds, in /proc/net/tcp (and tcp6), the socket from
// 127.0.0.1:from to 127.0.0.1:to.
func socketInode(from, to int) string {
	want := fmt.Sprintf(":%04X", from)
	peer := fmt.Sprintf(":%04X", to)
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(table)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fl := strings.Fields(sc.Text())
			if len(fl) < 10 || !strings.HasSuffix(fl[1], want) || !strings.HasSuffix(fl[2], peer) {
				continue
			}
			f.Close()
			return fl[9]
		}
		f.Close()
	}
	return ""
}

// ownerOf finds the process with the socket open (only the user's own
// processes are readable, which is all that can matter).
func ownerOf(inode string) int {
	target := "socket:[" + inode + "]"
	procs, _ := filepath.Glob("/proc/[0-9]*")
	for _, p := range procs {
		fds, err := os.ReadDir(p + "/fd")
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if l, err := os.Readlink(p + "/fd/" + fd.Name()); err == nil && l == target {
				pid, _ := strconv.Atoi(filepath.Base(p))
				return pid
			}
		}
	}
	return 0
}

// descendsFrom reports whether pid is ancestor or a process under it.
func descendsFrom(pid, ancestor int) bool {
	for i := 0; i < 64 && pid > 1; i++ {
		if pid == ancestor {
			return true
		}
		pid = parentOf(pid)
	}
	return false
}

func parentOf(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// The name is in parentheses and may contain spaces; ppid follows it.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	fl := strings.Fields(s[i+1:])
	if len(fl) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(fl[1])
	return ppid
}
