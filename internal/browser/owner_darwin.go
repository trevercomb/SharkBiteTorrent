//go:build darwin && cgo

package browser

import (
	"crypto/rand"

	"sharkbite/internal/socks"
)

// GuardProxy sets up the browser window's proxy s and the window's options
// o so that only the window can use the proxy. On macOS WebKit's network
// process is started by launchd, not by the app, so who's connecting can't
// be traced back to the app as on Windows and Linux; WebKit can log in to a
// SOCKS proxy, though, so the window gets a password of its own.
func GuardProxy(s *socks.Server, o *Options) {
	s.Username, s.Password = "sharkbite", rand.Text()
	o.ProxyUser, o.ProxyPass = s.Username, s.Password
}
