package dashboardgen

import "testing"

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
