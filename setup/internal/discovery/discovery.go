// Package discovery finds Claude Code configuration directories on this
// machine and identifies the account behind each one.
package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Account is one discovered Claude Code configuration directory.
type Account struct {
	Dir   string // absolute path to the config directory (contains "projects/")
	Email string // "" if oauthAccount.emailAddress is missing or unreadable
}

type claudeJSON struct {
	OauthAccount struct {
		EmailAddress string `json:"emailAddress"`
	} `json:"oauthAccount"`
}

// Find scans home for Claude Code config directories: "home/.claude" and
// "home/.claude-*", each of which must contain a "projects" subdirectory to
// count. Results are sorted with ".claude" first, then the ".claude-*"
// variants alphabetically. Each Account's Email is read from
// "<dir>/.claude.json"; a missing or unparsable file just leaves it "".
func Find(home string) ([]Account, error) {
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil, err
	}

	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name != ".claude" && !strings.HasPrefix(name, ".claude-") {
			continue
		}
		full := filepath.Join(home, name)
		if info, err := os.Stat(filepath.Join(full, "projects")); err != nil || !info.IsDir() {
			continue
		}
		dirs = append(dirs, full)
	}

	sort.Slice(dirs, func(i, j int) bool {
		bi, bj := filepath.Base(dirs[i]), filepath.Base(dirs[j])
		if bi == ".claude" {
			return true
		}
		if bj == ".claude" {
			return false
		}
		return bi < bj
	})

	accounts := make([]Account, 0, len(dirs))
	for _, d := range dirs {
		accounts = append(accounts, Account{Dir: d, Email: readEmail(d)})
	}
	return accounts, nil
}

func readEmail(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		return ""
	}
	var cfg claudeJSON
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	return cfg.OauthAccount.EmailAddress
}
