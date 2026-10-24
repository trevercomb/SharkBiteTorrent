//go:build windows

package browser

import (
	"net"
	"os"
	"testing"
)

func TestFromThisApp(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		got <- c
	}()
	out, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	in := <-got
	defer in.Close()

	from := uint16(out.LocalAddr().(*net.TCPAddr).Port)
	to := uint16(ln.Addr().(*net.TCPAddr).Port)
	if pid, ok := connOwner(from, to); !ok || pid != uint32(os.Getpid()) {
		t.Fatalf("owner of our own connection: %d %v (we're %d)", pid, ok, os.Getpid())
	}
	if !FromThisApp(in) {
		t.Error("our own connection isn't from this app")
	}
	if descendsFromUs(4) { // the System process
		t.Error("System counts as ours")
	}
}
