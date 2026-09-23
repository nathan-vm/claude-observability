// Package dashboardgen ports dashboard-generator.mjs: one Grafana
// dashboard per Claude Code account, scoped from a shared template.
package dashboardgen

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"claude-observability-dash-generator/internal/lokiclient"
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

func discoverAccounts(lokiURL string) ([]string, error) {
	series, err := lokiclient.Query(lokiURL,
		"sum by (user_email) (count_over_time({service_name=\"claude-code\"} | event_name = `api_request` [7d]))",
		time.Now().Unix())
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, s := range series {
		if e := s.Metric["user_email"]; e != "" {
			set[e] = true
		}
	}
	emails := make([]string, 0, len(set))
	for e := range set {
		emails = append(emails, e)
	}
	sort.Strings(emails)
	return emails, nil
}

// rateCutlines computes the P75/outlier/extreme thresholds over an
// account's last 7 days of the given rate field ("rate", "rate_input", or
// "rate_output"), counting only buckets with actual request activity (an
// EWMA never quite reaches zero, so idle buckets would pull the quantiles
// down to meaninglessness). Falls back to fixed defaults on any Loki
// error or fewer than 20 qualifying samples.
func rateCutlines(lokiURL, exporterStream, rateHalfLife, email, field string) Cutlines {
	pattern := escapeRegex(email)
	end := time.Now().Unix()
	start := end - 7*24*3600

	smoothedExpr := fmt.Sprintf(
		"sum(last_over_time({service_name=\"claude-code-rate\", halflife=\"%s\"} | user_email =~ `%s` | unwrap %s [5m]) by (user_email))",
		rateHalfLife, pattern, field)
	activeExpr := fmt.Sprintf(
		"sum(count_over_time({service_name=\"claude-code\"} | event_name = `api_request` | user_email =~ `%s` [5m]))",
		pattern)

	smoothed, errS := lokiclient.QueryRange(lokiURL, smoothedExpr, start, end, 300)
	active, errA := lokiclient.QueryRange(lokiURL, activeExpr, start, end, 300)
	if errS != nil || errA != nil {
		return cutlineFallback
	}

	busy := map[string]bool{}
	if len(active) > 0 {
		for _, v := range active[0].Values {
			if count, err := strconv.ParseFloat(v[1], 64); err == nil && count > 0 {
				busy[v[0]] = true
			}
		}
	}
	var values []float64
	if len(smoothed) > 0 {
		for _, v := range smoothed[0].Values {
			if !busy[v[0]] {
				continue
			}
			val, err := strconv.ParseFloat(v[1], 64)
			if err != nil || math.IsInf(val, 0) || !(val > 0) {
				continue
			}
			values = append(values, val)
		}
	}
	sort.Float64s(values)
	if len(values) < 20 {
		return cutlineFallback
	}
	q1, _ := quantile(values, 0.25)
	q3, _ := quantile(values, 0.75)
	iqr := q3 - q1
	return Cutlines{
		P75:      int64(math.Round(q3)),
		Outlier:  int64(math.Round(q3 + 1.5*iqr)),
		Extreme:  int64(math.Round(q3 + 3*iqr)),
		Fallback: false,
	}
}

// Option is one dropdown-filter choice (a value plus its display text).
type Option struct {
	Text, Value string
}

// skillOwners lists the plugin-skill owners an account used over the last
// 30 days. "local" (no plugin prefix) gets a friendlier display label.
func skillOwners(lokiURL, exporterStream, email string) []Option {
	expr := fmt.Sprintf(
		"sum by (skill_owner) (count_over_time({service_name=\"%s\", kind=\"skills\"} | user_email =~ `%s` [1h]))",
		exporterStream, escapeRegex(email))
	end := time.Now().Unix()
	result, err := lokiclient.QueryRange(lokiURL, expr, end-30*24*3600, end, 3600)
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, s := range result {
		if o := s.Metric["skill_owner"]; o != "" {
			set[o] = true
		}
	}
	owners := make([]string, 0, len(set))
	for o := range set {
		owners = append(owners, o)
	}
	sort.Strings(owners)
	out := make([]Option, 0, len(owners))
	for _, o := range owners {
		text := o
		if o == "local" {
			text = "local (no plugin)"
		}
		out = append(out, Option{Text: text, Value: escapeRegex(o)})
	}
	return out
}

// mcpServers lists the MCP servers an account used over the last 30 days.
func mcpServers(lokiURL, exporterStream, email string) []string {
	expr := fmt.Sprintf(
		"sum by (mcp_server) (count_over_time({service_name=\"%s\", kind=\"tools\"} | user_email =~ `%s` | tool_source = `mcp` [1h]))",
		exporterStream, escapeRegex(email))
	end := time.Now().Unix()
	result, err := lokiclient.QueryRange(lokiURL, expr, end-30*24*3600, end, 3600)
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, s := range result {
		if m := s.Metric["mcp_server"]; m != "" {
			set[m] = true
		}
	}
	servers := make([]string, 0, len(set))
	for m := range set {
		servers = append(servers, m)
	}
	sort.Strings(servers)
	return servers
}

func accountVariable(email string) map[string]interface{} {
	escaped := escapeRegex(email)
	return map[string]interface{}{
		"name": "account", "label": "Account", "type": "constant",
		"query":   escaped,
		"current": map[string]interface{}{"text": email, "value": escaped},
		"hide":    float64(2),
	}
}

// AllCutlines bundles the three cutline sets scopeAccount needs: the total
// rate panel's, and the input/output split panel's own.
type AllCutlines struct {
	Total, Input, Output Cutlines
}

// applyCutlines injects the cutlines into every timeseries panel's
// threshold steps that declare them — the total panel's cutlines live in
// fieldConfig.defaults, the per-series (input/output) ones live in a
// fieldConfig.overrides entry keyed by the override's matcher.options.
func applyCutlines(dashboard map[string]interface{}, byName map[string]Cutlines) {
	write := func(steps []interface{}, cut Cutlines, has bool) {
		if steps == nil || !has {
			return
		}
		values := []int64{cut.P75, cut.Outlier, cut.Extreme}
		for i := 1; i < len(steps) && i <= len(values); i++ {
			if step, ok := steps[i].(map[string]interface{}); ok {
				step["value"] = values[i-1]
			}
		}
	}
	for _, panel := range allPanels(dashboard) {
		if panel["type"] != "timeseries" {
			continue
		}
		fieldConfig, _ := panel["fieldConfig"].(map[string]interface{})
		if fieldConfig == nil {
			continue
		}
		if defaults, ok := fieldConfig["defaults"].(map[string]interface{}); ok {
			if thresholds, ok := defaults["thresholds"].(map[string]interface{}); ok {
				steps, _ := thresholds["steps"].([]interface{})
				total, has := byName["total"]
				write(steps, total, has)
			}
		}
		overrides, _ := fieldConfig["overrides"].([]interface{})
		for _, o := range overrides {
			override, ok := o.(map[string]interface{})
			if !ok {
				continue
			}
			matcher, _ := override["matcher"].(map[string]interface{})
			series, _ := matcher["options"].(string)
			properties, _ := override["properties"].([]interface{})
			for _, p := range properties {
				prop, ok := p.(map[string]interface{})
				if !ok || prop["id"] != "thresholds" {
					continue
				}
				value, _ := prop["value"].(map[string]interface{})
				steps, _ := value["steps"].([]interface{})
				cut, has := byName[series]
				write(steps, cut, has)
			}
		}
	}
}

// AccountLimits are the block_5h/week token-limit references drawn on the
// gauges. No longer auto-calibrated by anything (usage-meter is gone) —
// purely user-set in account-limits.json, or the hardcoded default.
type AccountLimits struct {
	Block5h, Week int64
}

func loadLimits(path string) (map[string]interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]interface{}{}, nil
		}
		return nil, fmt.Errorf("account-limits.json unreadable (%w); fix the file: carrying on without it would revert limits and the ignore list", err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("account-limits.json unreadable (%w); fix the file: carrying on without it would revert limits and the ignore list", err)
	}
	return doc, nil
}

func limitsFor(doc map[string]interface{}, email string) AccountLimits {
	result := AccountLimits{Block5h: 1_750_000, Week: 21_500_000}
	if def, ok := doc["default"].(map[string]interface{}); ok {
		if v, ok := def["block_5h"].(float64); ok {
			result.Block5h = int64(v)
		}
		if v, ok := def["week"].(float64); ok {
			result.Week = int64(v)
		}
	}
	if accounts, ok := doc["accounts"].(map[string]interface{}); ok {
		if acct, ok := accounts[email].(map[string]interface{}); ok {
			if v, ok := acct["block_5h"].(float64); ok {
				result.Block5h = int64(v)
			}
			if v, ok := acct["week"].(float64); ok {
				result.Week = int64(v)
			}
		}
	}
	return result
}

// scopeAccount deep-clones template and mutates the clone: pins the
// account, injects cutlines into thresholds AND into the LogQL expressions
// themselves (the rate panel filters by cutline in-query, not just at
// display time), sets the limit-reference textboxes, rewrites drill-down
// links to point at this account's own uid, and replaces the MCP-
// server/skill-owner filter options with what this account actually used.
func scopeAccount(template map[string]interface{}, email string, cutlines AllCutlines,
	servers []string, owners []Option, limits AccountLimits, exporterStream, rateHalfLife string,
) (map[string]interface{}, error) {
	data, err := json.Marshal(template)
	if err != nil {
		return nil, err
	}
	var dashboard map[string]interface{}
	if err := json.Unmarshal(data, &dashboard); err != nil {
		return nil, err
	}

	uid := "cc-" + slug(email)
	if len(uid) > 40 {
		uid = uid[:40]
	}
	dashboard["uid"] = uid
	dashboard["title"] = "Claude Code — " + email

	if cutlines.Total.Fallback {
		dashboard["description"] = fmt.Sprintf(
			"Account %s. Generated from templates/claude-code.json — do not edit by hand, run generate-account-dashboards.mjs. Rate cutlines: DEFAULT values (not enough history, or Loki unavailable), not computed from this account.",
			email)
	} else {
		dashboard["description"] = fmt.Sprintf(
			"Account %s. Generated from templates/claude-code.json — do not edit by hand, run generate-account-dashboards.mjs. Rate cutlines, over this account's last 7 days: P75 %s, outlier %s, extreme %s tokens/h.",
			email, commaFormat(cutlines.Total.P75), commaFormat(cutlines.Total.Outlier), commaFormat(cutlines.Total.Extreme))
	}

	replaceVariable(dashboard, accountVariable(email))
	applyCutlines(dashboard, map[string]Cutlines{"total": cutlines.Total, "input": cutlines.Input, "output": cutlines.Output})

	for _, panel := range allPanels(dashboard) {
		targets, _ := panel["targets"].([]interface{})
		for _, t := range targets {
			target, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			expr, ok := target["expr"].(string)
			if !ok || expr == "" {
				continue
			}
			expr = strings.ReplaceAll(expr, "__EXPORTER_STREAM__", exporterStream)
			expr = strings.ReplaceAll(expr, "__HALFLIFE__", rateHalfLife)
			expr = strings.ReplaceAll(expr, "__CUT_P75__", strconv.FormatInt(cutlines.Total.P75, 10))
			expr = strings.ReplaceAll(expr, "__CUT_OUTLIER__", strconv.FormatInt(cutlines.Total.Outlier, 10))
			expr = strings.ReplaceAll(expr, "__CUT_EXTREME__", strconv.FormatInt(cutlines.Total.Extreme, 10))
			target["expr"] = expr
		}
	}

	for _, nv := range []struct {
		name  string
		value int64
	}{{"limit_tokens_5h", limits.Block5h}, {"limit_tokens_week", limits.Week}} {
		v := strconv.FormatInt(nv.value, 10)
		replaceVariable(dashboard, map[string]interface{}{
			"name": nv.name, "type": "textbox", "hide": float64(2),
			"query": v, "current": map[string]interface{}{"text": v, "value": v},
		})
	}

	for _, panel := range allPanels(dashboard) {
		var linkLists [][]interface{}
		fieldConfig, _ := panel["fieldConfig"].(map[string]interface{})
		if defaults, ok := fieldConfig["defaults"].(map[string]interface{}); ok {
			if links, ok := defaults["links"].([]interface{}); ok {
				linkLists = append(linkLists, links)
			}
		}
		if overrides, ok := fieldConfig["overrides"].([]interface{}); ok {
			for _, o := range overrides {
				override, ok := o.(map[string]interface{})
				if !ok {
					continue
				}
				properties, _ := override["properties"].([]interface{})
				for _, p := range properties {
					prop, ok := p.(map[string]interface{})
					if !ok || prop["id"] != "links" {
						continue
					}
					if links, ok := prop["value"].([]interface{}); ok {
						linkLists = append(linkLists, links)
					}
				}
			}
		}
		for _, links := range linkLists {
			for _, l := range links {
				link, ok := l.(map[string]interface{})
				if !ok {
					continue
				}
				url, ok := link["url"].(string)
				if !ok || !strings.Contains(url, "__DASHBOARD__") {
					continue
				}
				link["url"] = strings.ReplaceAll(url, "__DASHBOARD__", uid)
			}
		}
	}

	filter := func(name, label string, options []Option) map[string]interface{} {
		all := append([]Option{{Text: "All", Value: ".*"}}, options...)
		queryParts := make([]string, len(all))
		optList := make([]interface{}, len(all))
		for i, o := range all {
			queryParts[i] = o.Text + " : " + o.Value
			optList[i] = map[string]interface{}{"text": o.Text, "value": o.Value, "selected": i == 0}
		}
		return map[string]interface{}{
			"name": name, "label": label, "type": "custom",
			"query":      strings.Join(queryParts, ","),
			"options":    optList,
			"current":    map[string]interface{}{"text": all[0].Text, "value": all[0].Value},
			"includeAll": false, "multi": false, "hide": float64(0),
		}
	}
	serverOptions := make([]Option, len(servers))
	for i, s := range servers {
		serverOptions[i] = Option{Text: s, Value: escapeRegex(s)}
	}
	replaceVariable(dashboard, filter("server", "MCP server", serverOptions))
	replaceVariable(dashboard, filter("owner", "Skill owner", owners))

	if err := assertNoPlaceholders(dashboard); err != nil {
		return nil, err
	}
	return dashboard, nil
}
