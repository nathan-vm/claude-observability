// Package streamid mints the random id the wizard uses to name a fresh
// install's exporter stream (EXPORTER_STREAM=claude-code-exporter-<id>).
//
// A random id per install, chosen once and never touched again (WriteBlock
// is a no-op on a re-run — see envwriter), replaces a shared sequential
// counter: nobody has to coordinate a number, and the reimport story (see
// README, "Rescan and reimport") is the same either way — generate a new
// one by hand and follow the same steps.
package streamid

import (
	"crypto/rand"
	"fmt"
)

// New returns a random UUIDv4 string, e.g. "f47ac10b-58cc-4372-a567-0e02b2c3d479".
func New() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("streamid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
