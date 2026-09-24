// Package accounts resolves every Claude Code config directory this
// machine monitors — port of accounts.mjs.
package accounts

import (
	"os"
	"path/filepath"
	"strings"
)

func expandHome(value, homeDir string) string {
	value = strings.ReplaceAll(value, "${HOME}", homeDir)
	value = strings.ReplaceAll(value, "$HOME", homeDir)
	return value
}

// ResolveConfigDirs returns every Claude Code config directory to scan:
// claudeDirEnv (default "<homeDir>/.claude") plus extraDirsEnv split on
// os.PathListSeparator — ":" on POSIX, ";" on Windows, same as $PATH —
// each with "${HOME}"/"$HOME" expanded, deduplicated, primary first.
func ResolveConfigDirs(homeDir, claudeDirEnv, extraDirsEnv string) []string {
	primary := claudeDirEnv
	if primary == "" {
		primary = filepath.Join(homeDir, ".claude")
	} else {
		primary = expandHome(primary, homeDir)
	}

	seen := map[string]bool{primary: true}
	out := []string{primary}
	for _, d := range strings.Split(extraDirsEnv, string(os.PathListSeparator)) {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		d = expandHome(d, homeDir)
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}
