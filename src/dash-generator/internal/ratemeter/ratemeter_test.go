package ratemeter

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"claude-observability-dash-generator/internal/state"
)

func TestEwma_ConvergesTowardConstantInput(t *testing.T) {
	points := make([]Point, 50)
	for i := range points {
		points[i] = Point{TsS: int64(i * 300), Value: 100}
	}
	out := ewma(points, 1200) // 4 buckets half-life
	last := out[len(out)-1].Value
	if math.Abs(last-100) > 1 {
		t.Errorf("ewma converged to %v, want ~100", last)
	}
	if out[0].Value >= out[len(out)/2].Value {
		t.Errorf("ewma should rise from 0 toward 100, got %v then %v", out[0].Value, out[len(out)/2].Value)
	}
}

func TestEscapeRegex(t *testing.T) {
	got := escapeRegex("a.b+c")
	if got != `a\.b\+c` {
		t.Errorf("escapeRegex() = %q", got)
	}
}

func TestPublishRate_NoAccountsReturnsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()

	st := &state.State{RatePublished: map[string]int64{}}
	n, err := PublishRate(PublishConfig{LokiURL: srv.URL, HalfLifeS: 1200, HalfLifeLabel: "20m", BackfillDays: 14}, st, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
}

func TestPublishRate_PublishesAndUpdatesDedupState(t *testing.T) {
	var pushed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/query") && !strings.Contains(r.URL.Path, "range"):
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"user_email":"a@example.com"}}
			]}}`))
		case strings.Contains(r.URL.Path, "query_range"):
			w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{},"values":[["1700000000","10"],["1700000300","20"]]}
			]}}`))
		case strings.Contains(r.URL.Path, "push"):
			pushed = true
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	st := &state.State{RatePublished: map[string]int64{}}
	n, err := PublishRate(PublishConfig{LokiURL: srv.URL, HalfLifeS: 1200, HalfLifeLabel: "20m", BackfillDays: 1}, st, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("n = 0, want points published")
	}
	if !pushed {
		t.Error("push endpoint was never called")
	}
	if st.RatePublished["20m:a@example.com"] == 0 {
		t.Error("dedup state not updated after publish")
	}
}

func TestPublishRate_DryRunDoesNotPush(t *testing.T) {
	var pushed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/query") && !strings.Contains(r.URL.Path, "range"):
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"user_email":"a@example.com"}}
			]}}`))
		case strings.Contains(r.URL.Path, "query_range"):
			w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{},"values":[["1700000000","10"]]}
			]}}`))
		case strings.Contains(r.URL.Path, "push"):
			pushed = true
		}
	}))
	defer srv.Close()

	st := &state.State{RatePublished: map[string]int64{}}
	_, err := PublishRate(PublishConfig{LokiURL: srv.URL, HalfLifeS: 1200, HalfLifeLabel: "20m", BackfillDays: 1, DryRun: true}, st, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if pushed {
		t.Error("push endpoint was called under DryRun")
	}
}
