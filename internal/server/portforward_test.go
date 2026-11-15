package server

import (
	"net"
	"strings"
	"testing"
)

func TestFreePortHonoursChoice(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	busy := ln.Addr().(*net.TCPAddr).Port
	if _, err := freePort(busy); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("busy chosen port: got %v", err)
	}
	ln.Close()
	if got, err := freePort(busy); err != nil || got != busy {
		t.Fatalf("free chosen port: got %d, %v", got, err)
	}
	if got, err := freePort(0); err != nil || got == 0 {
		t.Fatalf("auto port: got %d, %v", got, err)
	}
}
