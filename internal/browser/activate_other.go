//go:build windows || (linux && cgo)

package browser

// letForward is for macOS (activate_darwin.go): elsewhere a window the
// app opens comes to the front by itself.
func letForward(int) {}
