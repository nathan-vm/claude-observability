// Package health provides a minimal, protocol-agnostic reachability check
// used by the setup wizard before it writes any configuration.
package health

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"
)

var defaultPorts = map[string]string{"https": "443", "http": "80"}

func hostPort(u *url.URL) (string, error) {
	if u.Hostname() == "" {
		return "", errors.New("no host (is the http:// or https:// scheme missing?)")
	}
	if u.Port() != "" {
		return u.Host, nil
	}
	port, ok := defaultPorts[u.Scheme]
	if !ok {
		return "", fmt.Errorf("no port and unsupported scheme %q (use http:// or https://, or add a port)", u.Scheme)
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

// Dial reports whether a TCP connection can be opened to the host:port
// encoded in rawURL, within timeout. A missing port defaults from the scheme
// (https 443, http 80). It knows nothing about what's on the
// other end — this is the same check whether rawURL points at a local
// docker stack or a remote collector.
func Dial(rawURL string, timeout time.Duration) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid endpoint %q: %w", rawURL, err)
	}
	addr, err := hostPort(u)
	if err != nil {
		return fmt.Errorf("invalid endpoint %q: %w", rawURL, err)
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", rawURL, err)
	}
	conn.Close()
	return nil
}
