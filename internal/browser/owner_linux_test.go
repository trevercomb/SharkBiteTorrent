//go:build linux

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
	if !FromThisApp(in) {
		t.Error("our own connection isn't from this app")
	}
	if descendsFrom(1, os.Getpid()) {
		t.Error("init counts as ours")
	}
	if !descendsFrom(os.Getpid(), os.Getpid()) {
		t.Error("we don't count as ours")
	}
	if !isWindowProgram(os.Getpid()) {
		t.Error("our own program isn't one the window runs")
	}
	if isWindowProgram(1) {
		t.Error("init runs a window program")
	}
}
