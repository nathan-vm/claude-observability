package health

import (
	"net"
	"net/url"
	"strings"
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

func TestDial_ReportsRealCause(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"ftp://localhost", "unsupported scheme"},
		{"localhost", "no host"},
		{"localhost:47100", "no host"},
		{"//localhost", "unsupported scheme"},
		{"https://", "no host"},
	}
	for _, tt := range tests {
		err := Dial(tt.raw, time.Second)
		if err == nil {
			t.Errorf("Dial(%q) = nil, want error", tt.raw)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Dial(%q) = %v, want it to mention %q", tt.raw, err, tt.want)
		}
	}
}

func TestHostPort(t *testing.T) {
	tests := []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{"https://otel.example.com", "otel.example.com:443", false},
		{"http://otel.example.com", "otel.example.com:80", false},
		{"https://otel.example.com/v1/logs", "otel.example.com:443", false},
		{"https://otel.example.com:8443", "otel.example.com:8443", false},
		{"http://localhost:4318", "localhost:4318", false},
		{"https://[::1]", "[::1]:443", false},
		{"ftp://example.com", "", true},
		{"example.com", "", true},
		{"https://", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			u, err := url.Parse(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			got, err := hostPort(u)
			if (err != nil) != tt.wantErr {
				t.Fatalf("hostPort(%q) err = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("hostPort(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestDial_InvalidURL(t *testing.T) {
	if err := Dial("://not a url", time.Second); err == nil {
		t.Error("Dial() = nil, want error for invalid url")
	}
}
