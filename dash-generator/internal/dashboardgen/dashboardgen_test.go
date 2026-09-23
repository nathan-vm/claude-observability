package dashboardgen

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"A@Example.com": "a-example-com",
		"--x--":          "x",
		"already-clean":  "already-clean",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEscapeRegex(t *testing.T) {
	if got := escapeRegex("a.b+c"); got != `a\.b\+c` {
		t.Errorf("escapeRegex() = %q", got)
	}
}

func TestQuantile_Interpolates(t *testing.T) {
	sorted := []float64{1, 2, 3, 4}
	got, ok := quantile(sorted, 0.5)
	if !ok || got != 2.5 {
		t.Errorf("quantile(0.5) = %v, %v, want 2.5, true", got, ok)
	}
}

func TestQuantile_EmptyReturnsFalse(t *testing.T) {
	if _, ok := quantile(nil, 0.5); ok {
		t.Error("quantile(nil) ok = true, want false")
	}
}

func TestCommaFormat(t *testing.T) {
	cases := map[int64]string{
		0:       "0",
		999:     "999",
		1000:    "1,000",
		1234567: "1,234,567",
		-1234:   "-1,234",
	}
	for in, want := range cases {
		if got := commaFormat(in); got != want {
			t.Errorf("commaFormat(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestAllPanels_WalksCollapsedRows(t *testing.T) {
	dashboard := map[string]interface{}{
		"panels": []interface{}{
			map[string]interface{}{"id": float64(1)},
			map[string]interface{}{
				"id": float64(2),
				"panels": []interface{}{
					map[string]interface{}{"id": float64(3)},
				},
			},
		},
	}
	panels := allPanels(dashboard)
	if len(panels) != 3 {
		t.Fatalf("got %d panels, want 3 (including nested row panel)", len(panels))
	}
}

func TestAssertNoPlaceholders_FailsOnUnreplaced(t *testing.T) {
	dashboard := map[string]interface{}{
		"uid": "test",
		"panels": []interface{}{
			map[string]interface{}{
				"id": float64(1),
				"targets": []interface{}{
					map[string]interface{}{"refId": "A", "expr": "sum(__EXPORTER_STREAM__)"},
				},
			},
		},
	}
	if err := assertNoPlaceholders(dashboard); err == nil {
		t.Error("assertNoPlaceholders() = nil, want error for unreplaced placeholder")
	}
}

func TestAssertNoPlaceholders_PassesWhenClean(t *testing.T) {
	dashboard := map[string]interface{}{
		"uid": "test",
		"panels": []interface{}{
			map[string]interface{}{
				"id": float64(1),
				"targets": []interface{}{
					map[string]interface{}{"refId": "A", "expr": "sum(claude-code-exporter-1)"},
				},
			},
		},
	}
	if err := assertNoPlaceholders(dashboard); err != nil {
		t.Errorf("assertNoPlaceholders() = %v, want nil", err)
	}
}

func TestReplaceVariable_InsertsNewAndReplacesExisting(t *testing.T) {
	dashboard := map[string]interface{}{}
	replaceVariable(dashboard, map[string]interface{}{"name": "account", "value": "1"})
	replaceVariable(dashboard, map[string]interface{}{"name": "account", "value": "2"})

	templating := dashboard["templating"].(map[string]interface{})
	list := templating["list"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("got %d variables, want 1 (replaced not duplicated)", len(list))
	}
	if list[0].(map[string]interface{})["value"] != "2" {
		t.Errorf("value = %v, want 2 (the replacement)", list[0])
	}
}

func TestDiscoverAccounts_ReturnsSortedUniqueEmails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"user_email":"b@example.com"}},
			{"metric":{"user_email":"a@example.com"}}
		]}}`))
	}))
	defer srv.Close()

	emails, err := discoverAccounts(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(emails) != 2 || emails[0] != "a@example.com" || emails[1] != "b@example.com" {
		t.Errorf("emails = %v", emails)
	}
}

func TestRateCutlines_ComputesFromBusyBucketsOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		switch {
		case strings.Contains(query, "claude-code-rate"):
			var vals []string
			for i := 1; i <= 25; i++ {
				vals = append(vals, fmt.Sprintf(`["%d","%d"]`, 1700000000+i*300, i))
			}
			w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{},"values":[` + strings.Join(vals, ",") + `]}
			]}}`))
		default:
			var vals []string
			for i := 1; i <= 25; i++ {
				vals = append(vals, fmt.Sprintf(`["%d","1"]`, 1700000000+i*300))
			}
			w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{},"values":[` + strings.Join(vals, ",") + `]}
			]}}`))
		}
	}))
	defer srv.Close()

	cut := rateCutlines(srv.URL, "claude-code-exporter-1", "20m", "a@example.com", "rate")
	if cut.Fallback {
		t.Fatal("got fallback cutlines, want computed (25 >= 20 samples)")
	}
	if cut.P75 <= 0 || cut.Outlier < cut.P75 || cut.Extreme < cut.Outlier {
		t.Errorf("cutlines not monotonically increasing: %+v", cut)
	}
}

func TestRateCutlines_FewSamplesFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{},"values":[["1700000000","5"]]}
		]}}`))
	}))
	defer srv.Close()

	cut := rateCutlines(srv.URL, "claude-code-exporter-1", "20m", "a@example.com", "rate")
	if !cut.Fallback || cut.P75 != cutlineFallback.P75 {
		t.Errorf("cutlines = %+v, want fallback", cut)
	}
}

func TestRateCutlines_LokiErrorFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cut := rateCutlines(srv.URL, "claude-code-exporter-1", "20m", "a@example.com", "rate")
	if !cut.Fallback {
		t.Error("want fallback on Loki error")
	}
}

func TestSkillOwners_LabelsLocalSpecially(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"skill_owner":"local"}},
			{"metric":{"skill_owner":"superpowers"}}
		]}}`))
	}))
	defer srv.Close()

	owners := skillOwners(srv.URL, "claude-code-exporter-1", "a@example.com")
	if len(owners) != 2 {
		t.Fatalf("got %d owners", len(owners))
	}
	if owners[0].Text != "local (no plugin)" {
		t.Errorf("owners[0] = %+v", owners[0])
	}
}

func TestMcpServers_ReturnsSortedNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"mcp_server":"github"}},
			{"metric":{"mcp_server":"filesystem"}}
		]}}`))
	}))
	defer srv.Close()

	servers := mcpServers(srv.URL, "claude-code-exporter-1", "a@example.com")
	if len(servers) != 2 || servers[0] != "filesystem" || servers[1] != "github" {
		t.Errorf("servers = %v", servers)
	}
}

func loadTestTemplate(t *testing.T) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "template.json"))
	if err != nil {
		t.Fatal(err)
	}
	var template map[string]interface{}
	if err := json.Unmarshal(data, &template); err != nil {
		t.Fatal(err)
	}
	return template
}

func TestScopeAccount_ReplacesAllPlaceholders(t *testing.T) {
	template := loadTestTemplate(t)
	cutlines := AllCutlines{
		Total:  Cutlines{P75: 100, Outlier: 200, Extreme: 300},
		Input:  Cutlines{P75: 10, Outlier: 20, Extreme: 30},
		Output: Cutlines{P75: 5, Outlier: 15, Extreme: 25},
	}
	limits := AccountLimits{Block5h: 1_750_000, Week: 21_500_000}

	dashboard, err := scopeAccount(template, "a@example.com", cutlines, []string{"github"},
		[]Option{{Text: "superpowers", Value: "superpowers"}}, limits, "claude-code-exporter-1", "20m")
	if err != nil {
		t.Fatal(err)
	}

	if dashboard["uid"] != "cc-a-example-com" {
		t.Errorf("uid = %v", dashboard["uid"])
	}
	panels := allPanels(dashboard)
	target := panels[0]["targets"].([]interface{})[0].(map[string]interface{})
	expr := target["expr"].(string)
	for _, placeholder := range []string{"__EXPORTER_STREAM__", "__HALFLIFE__", "__CUT_P75__", "__CUT_OUTLIER__", "__CUT_EXTREME__"} {
		if strings.Contains(expr, placeholder) {
			t.Errorf("expr still contains %s: %s", placeholder, expr)
		}
	}
	if !strings.Contains(expr, "claude-code-exporter-1") || !strings.Contains(expr, "20m") {
		t.Errorf("expr missing substituted values: %s", expr)
	}

	links := panels[0]["fieldConfig"].(map[string]interface{})["defaults"].(map[string]interface{})["links"].([]interface{})
	link := links[0].(map[string]interface{})
	if link["url"] != "/d/cc-a-example-com" {
		t.Errorf("link url = %v", link["url"])
	}

	steps := panels[0]["fieldConfig"].(map[string]interface{})["defaults"].(map[string]interface{})["thresholds"].(map[string]interface{})["steps"].([]interface{})
	if steps[1].(map[string]interface{})["value"] != int64(100) {
		t.Errorf("total P75 threshold not applied: %v", steps[1])
	}
}

func TestScopeAccount_OriginalTemplateUntouched(t *testing.T) {
	template := loadTestTemplate(t)
	original, _ := json.Marshal(template)

	cutlines := AllCutlines{Total: Cutlines{P75: 1, Outlier: 2, Extreme: 3}}
	_, err := scopeAccount(template, "a@example.com", cutlines, nil, nil, AccountLimits{}, "s", "20m")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(template)
	if string(original) != string(after) {
		t.Error("scopeAccount mutated the shared template — must deep-clone first")
	}
}

func TestLimitsFor_FallsBackThroughDefaultThenAccount(t *testing.T) {
	doc := map[string]interface{}{
		"default":  map[string]interface{}{"block_5h": float64(111), "week": float64(222)},
		"accounts": map[string]interface{}{"a@example.com": map[string]interface{}{"block_5h": float64(999)}},
	}
	got := limitsFor(doc, "a@example.com")
	if got.Block5h != 999 || got.Week != 222 {
		t.Errorf("got %+v, want block_5h from account, week from default", got)
	}

	gotOther := limitsFor(doc, "nobody@example.com")
	if gotOther.Block5h != 111 || gotOther.Week != 222 {
		t.Errorf("got %+v for unconfigured account, want the default block", gotOther)
	}
}

func TestLimitsFor_HardcodedFallbackWhenNoDefaultEither(t *testing.T) {
	got := limitsFor(map[string]interface{}{}, "a@example.com")
	if got.Block5h != 1_750_000 || got.Week != 21_500_000 {
		t.Errorf("got %+v, want hardcoded fallback", got)
	}
}

func TestLoadLimits_MissingFileReturnsEmptyDoc(t *testing.T) {
	doc, err := loadLimits(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc) != 0 {
		t.Errorf("doc = %v, want empty", doc)
	}
}
