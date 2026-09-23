// Package dashboardgen ports dashboard-generator.mjs: one Grafana
// dashboard per Claude Code account, scoped from a shared template.
package dashboardgen

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Cutlines are the three rate-panel threshold lines for one account:
// P75, Tukey's inner fence ("outlier"), and outer fence ("extreme").
// Fallback is true when there wasn't enough history to compute them for
// real, and CUTLINE_FALLBACK's defaults were used instead.
type Cutlines struct {
	P75, Outlier, Extreme int64
	Fallback              bool
}

var cutlineFallback = Cutlines{P75: 766_008, Outlier: 1_721_058, Extreme: 2_676_108, Fallback: true}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func slug(email string) string {
	s := nonAlnum.ReplaceAllString(strings.ToLower(email), "-")
	return strings.Trim(s, "-")
}

func escapeRegex(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '.', '+', '*', '?', '(', ')', '|', '[', ']', '{', '}', '\\', '^', '$', '`':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func quantile(sorted []float64, q float64) (float64, bool) {
	if len(sorted) == 0 {
		return 0, false
	}
	pos := float64(len(sorted)-1) * q
	low := int(pos)
	high := low
	if frac := pos - float64(low); frac > 0 {
		high = low + 1
	}
	if low == high {
		return sorted[low], true
	}
	return sorted[low] + (sorted[high]-sorted[low])*(pos-float64(low)), true
}

func commaFormat(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// allPanels walks every panel, including ones nested inside a collapsed
// row (row.panels, not the top-level list) — a panel inside a collapsed
// row that's missed here would silently never get its placeholders
// replaced.
func allPanels(node map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	panels, _ := node["panels"].([]interface{})
	for _, p := range panels {
		panel, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		out = append(out, panel)
		out = append(out, allPanels(panel)...)
	}
	return out
}

var placeholderRe = regexp.MustCompile(`__[A-Z_]+__`)

// assertNoPlaceholders fails loudly if any __PLACEHOLDER__ survived
// substitution — writing a dashboard whose panel queries Loki for a
// literal placeholder string would just show "No data" with no warning.
func assertNoPlaceholders(dashboard map[string]interface{}) error {
	uid, _ := dashboard["uid"].(string)
	var left []string
	for _, panel := range allPanels(dashboard) {
		targets, _ := panel["targets"].([]interface{})
		for _, t := range targets {
			target, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			expr, _ := target["expr"].(string)
			if placeholderRe.MatchString(expr) {
				refID, _ := target["refId"].(string)
				left = append(left, fmt.Sprintf("%v:%s", panel["id"], refID))
			}
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("unreplaced placeholder in %s (dashboard %s) — that panel would show no data",
			strings.Join(left, ", "), uid)
	}
	return nil
}

// replaceVariable upserts one templating variable by name.
func replaceVariable(dashboard, variable map[string]interface{}) {
	templating, ok := dashboard["templating"].(map[string]interface{})
	if !ok {
		templating = map[string]interface{}{}
		dashboard["templating"] = templating
	}
	list, _ := templating["list"].([]interface{})
	name := variable["name"]
	idx := -1
	for i, item := range list {
		if m, ok := item.(map[string]interface{}); ok && m["name"] == name {
			idx = i
			break
		}
	}
	if idx >= 0 {
		list[idx] = variable
	} else {
		list = append([]interface{}{variable}, list...)
	}
	templating["list"] = list
}
