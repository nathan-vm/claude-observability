// Package shelltok names every recognizable sub-command in a Bash tool's
// command string — port of transcript-scan.mjs's tokenizeShell/
// extractBashCommands (collector-old/transcript-scan.mjs lines 200-349).
package shelltok

import (
	"regexp"
	"strings"
)

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// heredocEnd finds where a heredoc body (the "<<" is at str[i]) ends: a
// line that is exactly the delimiter, optionally indented when the
// redirect is "<<-". Returns -1 when this isn't actually a heredoc (a
// bare "<<" with no delimiter word, or one whose closing line never
// appears — the safer fallback then is ordinary text).
func heredocEnd(str string, i int) int {
	j := i + 2
	if j < len(str) && str[j] == '-' {
		j++
	}
	for j < len(str) && (str[j] == ' ' || str[j] == '\t') {
		j++
	}
	var delim strings.Builder
	if j < len(str) && (str[j] == '"' || str[j] == '\'') {
		q := str[j]
		j++
		for j < len(str) && str[j] != q {
			delim.WriteByte(str[j])
			j++
		}
		j++
	} else {
		for j < len(str) && !isSpaceByte(str[j]) && str[j] != ';' && str[j] != '&' && str[j] != '|' {
			delim.WriteByte(str[j])
			j++
		}
	}
	if delim.Len() == 0 {
		return -1
	}
	bodyStart := strings.IndexByte(str[j:], '\n')
	if bodyStart < 0 {
		return -1
	}
	bodyStart += j
	closeRe := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(delim.String()) + `[ \t]*$`)
	loc := closeRe.FindStringIndex(str[bodyStart+1:])
	if loc == nil {
		return -1
	}
	return bodyStart + 1 + loc[1]
}

// tokenizeShell tokenizes a shell command into words, grouped into
// top-level segments split on &&, ||, ; and |. Quotes/backticks and
// parens (what makes $(...) / (...) atomic) suppress splitting inside
// them; a heredoc gets the same atomic treatment for its whole body.
func tokenizeShell(str string) [][]string {
	segments := [][]string{nil}
	var word strings.Builder
	var quote byte
	depth := 0
	endWord := func() {
		if word.Len() > 0 {
			last := len(segments) - 1
			segments[last] = append(segments[last], word.String())
			word.Reset()
		}
	}
	newSegment := func() { segments = append(segments, nil) }

	for i := 0; i < len(str); i++ {
		ch := str[i]
		if quote != 0 {
			word.WriteByte(ch)
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '"', '\'', '`':
			quote = ch
			word.WriteByte(ch)
			continue
		case '(':
			depth++
			word.WriteByte(ch)
			continue
		case ')':
			if depth > 0 {
				depth--
			}
			word.WriteByte(ch)
			continue
		}
		if depth > 0 {
			word.WriteByte(ch)
			continue
		}
		if ch == '<' && i+1 < len(str) && str[i+1] == '<' {
			if end := heredocEnd(str, i); end >= 0 {
				word.WriteString(str[i:end])
				i = end - 1
				continue
			}
		}
		if ch == '\\' && i+1 < len(str) && str[i+1] == '\n' {
			i++
			continue
		}
		if ch == '\n' {
			endWord()
			newSegment()
			continue
		}
		if isSpaceByte(ch) {
			endWord()
			continue
		}
		if ch == '&' && i+1 < len(str) && str[i+1] == '&' {
			endWord()
			newSegment()
			i++
			continue
		}
		if ch == '|' && i+1 < len(str) && str[i+1] == '|' {
			endWord()
			newSegment()
			i++
			continue
		}
		if ch == ';' || ch == '|' {
			endWord()
			newSegment()
			continue
		}
		word.WriteByte(ch)
	}
	endWord()
	return segments
}

var shellKeywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true,
	"for": true, "while": true, "until": true, "do": true, "done": true,
	"case": true, "esac": true, "function": true, "select": true, "in": true, "time": true,
}

var commandNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./@:-]*$`)

func looksLikeCommandName(name string) bool {
	return !shellKeywords[name] && commandNameRe.MatchString(name)
}

var envAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// ExtractBashCommands names every recognizable sub-command in a Bash
// tool's `input.command` string ("git status && ls -la" -> ["git","ls"]).
// The "rtk" PreToolUse hook on this machine rewrites recognized commands
// before they execute ("git status" -> "rtk git status") — that prefix is
// unwrapped so calls attribute to the real command, not "rtk".
func ExtractBashCommands(commandStr string) []string {
	if commandStr == "" {
		return nil
	}
	var names []string
	for _, tokens := range tokenizeShell(commandStr) {
		i := 0
		for i < len(tokens) && (tokens[i] == `\` || envAssignRe.MatchString(tokens[i])) {
			i++
		}
		if i >= len(tokens) {
			continue
		}
		name := tokens[i]
		if name == "rtk" && i+1 < len(tokens) {
			name = tokens[i+1]
		}
		if name != "" && looksLikeCommandName(name) {
			names = append(names, name)
		}
	}
	return names
}
