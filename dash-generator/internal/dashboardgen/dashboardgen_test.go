package dashboardgen

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
