package health

import (
	"net"
	"testing"
	"time"
)

func TestDial_Success(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if err := Dial("http://"+ln.Addr().String(), time.Second); err != nil {
		t.Errorf("Dial() = %v, want nil", err)
	}
}

func TestDial_Failure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // now nothing is listening on addr

	if err := Dial("http://"+addr, 500*time.Millisecond); err == nil {
		t.Error("Dial() = nil, want error for closed port")
	}
}

func TestDial_NoPort(t *testing.T) {
	if err := Dial("http://localhost", time.Second); err == nil {
		t.Error("Dial() = nil, want error for missing port")
	}
}

func TestDial_InvalidURL(t *testing.T) {
	if err := Dial("://not a url", time.Second); err == nil {
		t.Error("Dial() = nil, want error for invalid url")
	}
}
