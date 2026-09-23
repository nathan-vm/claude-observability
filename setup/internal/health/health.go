// Package health provides a minimal, protocol-agnostic reachability check
// used by the setup wizard before it writes any configuration.
package health

import (
	"fmt"
	"net"
	"net/url"
	"time"
)

// Dial reports whether a TCP connection can be opened to the host:port
// encoded in rawURL, within timeout. It knows nothing about what's on the
// other end — this is the same check whether rawURL points at a local
// docker stack or a remote collector.
func Dial(rawURL string, timeout time.Duration) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid endpoint %q: %w", rawURL, err)
	}
	if u.Port() == "" {
		return fmt.Errorf("endpoint %q has no port", rawURL)
	}
	conn, err := net.DialTimeout("tcp", u.Host, timeout)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", rawURL, err)
	}
	conn.Close()
	return nil
}
