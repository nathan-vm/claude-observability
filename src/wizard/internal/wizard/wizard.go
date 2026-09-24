// Package wizard implements the interactive prompts the setup binary shows:
// which accounts to monitor, and the OTel endpoint/token to use.
package wizard

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"claude-observability-wizard/internal/discovery"
)

// AskLine prints "label [def]: " to w, reads one line from r, and returns
// the trimmed input, or def if the input was empty.
func AskLine(w io.Writer, r *bufio.Reader, label, def string) string {
	fmt.Fprintf(w, "%s [%s]: ", label, def)
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// AskYesNo prints prompt with a [y/N] or [Y/n] hint depending on
// defaultYes, reads one line from r, and returns true iff the input starts
// with 'y'/'Y' (empty input returns defaultYes).
func AskYesNo(w io.Writer, r *bufio.Reader, prompt string, defaultYes bool) bool {
	hint := "y/N"
	if defaultYes {
		hint = "Y/n"
	}
	fmt.Fprintf(w, "%s [%s] ", prompt, hint)
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultYes
	}
	return line[0] == 'y' || line[0] == 'Y'
}

// ChooseAccounts prints the discovered accounts and asks which to monitor.
// A single discovered account is chosen automatically, without prompting.
func ChooseAccounts(w io.Writer, r *bufio.Reader, accounts []discovery.Account) (chosen, rejected []discovery.Account) {
	if len(accounts) == 0 {
		return nil, nil
	}
	if len(accounts) == 1 {
		fmt.Fprintln(w, "  Only one directory found — using it.")
		return accounts, nil
	}

	fmt.Fprintln(w, "  Found:")
	for i, a := range accounts {
		email := a.Email
		if email == "" {
			email = "(unidentified account)"
		}
		fmt.Fprintf(w, "    [%d] %-24s %s\n", i+1, a.Dir, email)
	}

	if AskYesNo(w, r, "  Monitor all of these accounts?", true) {
		return accounts, nil
	}
	for _, a := range accounts {
		email := a.Email
		if email == "" {
			email = "(unidentified account)"
		}
		if AskYesNo(w, r, fmt.Sprintf("    Monitor %s?", email), true) {
			chosen = append(chosen, a)
		} else {
			rejected = append(rejected, a)
		}
	}
	return chosen, rejected
}
