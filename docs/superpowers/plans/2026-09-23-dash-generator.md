# Dash Generator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port `rate-meter.mjs` + `dashboard-generator.mjs` to a self-contained Go binary (`dash-generator`) with byte-for-byte equivalent behavior, and repoint the wizard's "install a background service" step at it instead of the old Node collector.

**Architecture:** A new Go module at `dash-generator/` with small, independently testable packages (`lokiclient`, `state`, `ratemeter`, `dashboardgen`) composed by `cmd/dash-generator/main.go`'s two loops. `setup/internal/service` is generalized from collector.mjs-specific to any executable, since both this and the future collector binary need it.

**Tech Stack:** Go 1.22 (standard library only), same GitHub Actions test/release pattern as `setup/`.

**Spec:** `docs/superpowers/specs/2026-09-23-dash-generator-design.md`

## Global Constraints

- No third-party Go modules — standard library only (matches `setup/`).
- Byte-for-byte equivalent LogQL queries, EWMA math, quantile/cutline math, and dashboard JSON mutation as `rate-meter.mjs`/`dashboard-generator.mjs` — this is a port, not a redesign.
- Dashboard JSON is manipulated as `map[string]interface{}`, never typed structs — a typed struct would silently drop unknown template fields on marshal.
- Must be run from the `claude-observability` repo root (same `docker-compose.yaml`-presence check as the wizard) — it needs `grafana/templates/`, `grafana/account-limits.json`, and writes into `grafana/dashboards/accounts/`.

---

## File Structure

```
dash-generator/
  go.mod
  cmd/dash-generator/main.go
  internal/lokiclient/lokiclient.go
  internal/lokiclient/lokiclient_test.go
  internal/state/state.go
  internal/state/state_test.go
  internal/ratemeter/ratemeter.go
  internal/ratemeter/ratemeter_test.go
  internal/dashboardgen/dashboardgen.go
  internal/dashboardgen/dashboardgen_test.go
  internal/dashboardgen/testdata/template.json      (fixture dashboard template)
.github/workflows/dash-generator-test.yml
.github/workflows/dash-generator-release.yml
setup/internal/service/service.go                   (generalized, modified)
setup/internal/service/service_darwin.go             (modified)
setup/internal/service/service_darwin_test.go        (modified)
setup/internal/service/service_linux.go              (modified)
setup/internal/service/service_linux_test.go         (modified)
setup/internal/service/service_windows.go            (modified)
setup/internal/service/service_windows_test.go       (modified)
setup/cmd/setup/main.go                              (modified: installService)
```

---

### Task 1: Go module scaffold + CI test workflow

**Files:**
- Create: `dash-generator/go.mod`
- Create: `dash-generator/cmd/dash-generator/main.go`
- Create: `.github/workflows/dash-generator-test.yml`

**Interfaces:**
- Produces: a `dash-generator` Go module (`module claude-observability-dash-generator`) later tasks add packages under; CI runs `go test ./...` inside `dash-generator/` on macOS/Linux/Windows on every push.

- [ ] **Step 1: Create the Go module**

```bash
mkdir -p dash-generator/cmd/dash-generator
cat > dash-generator/go.mod <<'EOF'
module claude-observability-dash-generator

go 1.22
EOF
```

- [ ] **Step 2: Placeholder main**

`dash-generator/cmd/dash-generator/main.go`:
```go
package main

import "fmt"

func main() {
	fmt.Println("dash-generator")
}
```

- [ ] **Step 3: Verify it builds and runs**

Run: `cd dash-generator && go build ./... && go run ./cmd/dash-generator`
Expected: prints `dash-generator`, no errors.

- [ ] **Step 4: Add a `dash-generator` job to the shared CI workflow**

One `.github/workflows/test.yml` for all three binaries (not a separate file per binary) — add a `dash-generator` job alongside the existing `setup` job, matrix'd the same way:
```yaml
  dash-generator:
    strategy:
      matrix:
        os: [macos-latest, ubuntu-latest, windows-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - run: go test ./...
        working-directory: dash-generator
```
(A third `collector` job, pointed at `working-directory: collector`, is added once that module exists — see the collector plan.)

- [ ] **Step 5: Commit**

```bash
git add dash-generator/go.mod dash-generator/cmd/dash-generator/main.go .github/workflows/dash-generator-test.yml
git commit -m "$(cat <<'EOF'
Scaffold dash-generator Go module and CI test matrix

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: `internal/lokiclient` — shared Loki HTTP client

**Files:**
- Create: `dash-generator/internal/lokiclient/lokiclient.go`
- Test: `dash-generator/internal/lokiclient/lokiclient_test.go`

**Interfaces:**
- Produces: `lokiclient.Series{Metric map[string]string, Values [][2]string}`, `lokiclient.Query(lokiURL, query string, atUnix int64) ([]Series, error)`, `lokiclient.QueryRange(lokiURL, query string, startUnix, endUnix, stepSeconds int64) ([]Series, error)`, `lokiclient.Stream{Labels map[string]string, Values []StreamValue}`, `lokiclient.StreamValue{TimestampNs, Line string, Metadata map[string]string}`, `lokiclient.Push(lokiURL string, streams []Stream) error`.

- [ ] **Step 1: Write the failing tests**

`dash-generator/internal/lokiclient/lokiclient_test.go`:
```go
package lokiclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQuery_ParsesInstantResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query" {
			t.Errorf("path = %s, want /loki/api/v1/query", r.URL.Path)
		}
		if r.URL.Query().Get("query") != "up" {
			t.Errorf("query param = %q, want up", r.URL.Query().Get("query"))
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"user_email":"a@example.com"},"value":["1700000000","42"]}
		]}}`))
	}))
	defer srv.Close()

	series, err := Query(srv.URL, "up", 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1", len(series))
	}
	if series[0].Metric["user_email"] != "a@example.com" {
		t.Errorf("metric = %v", series[0].Metric)
	}
	if series[0].Values[0] != [2]string{"1700000000", "42"} {
		t.Errorf("values = %v", series[0].Values)
	}
}

func TestQuery_NonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"error"}`))
	}))
	defer srv.Close()

	if _, err := Query(srv.URL, "up", 1700000000); err == nil {
		t.Error("Query() = nil error, want error for status=error")
	}
}

func TestQuery_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer srv.Close()

	if _, err := Query(srv.URL, "up", 1700000000); err == nil {
		t.Error("Query() = nil error, want error for 500")
	}
}

func TestQueryRange_ParsesMatrixResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			t.Errorf("path = %s", r.URL.Path)
		}
		for _, want := range []string{"query", "start", "end", "step"} {
			if r.URL.Query().Get(want) == "" {
				t.Errorf("missing query param %q", want)
			}
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{},"values":[["1700000000","1"],["1700000300","2"]]}
		]}}`))
	}))
	defer srv.Close()

	series, err := QueryRange(srv.URL, "up", 1700000000, 1700000600, 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || len(series[0].Values) != 2 {
		t.Fatalf("got %+v", series)
	}
}

func TestPush_SendsExpectedBody(t *testing.T) {
	var received map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/push" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := Push(srv.URL, []Stream{{
		Labels: map[string]string{"service_name": "x"},
		Values: []StreamValue{{TimestampNs: "123000000000", Line: "rate", Metadata: map[string]string{"rate": "5"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	streams, _ := received["streams"].([]interface{})
	if len(streams) != 1 {
		t.Fatalf("received streams = %v", received)
	}
}

func TestPush_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad"))
	}))
	defer srv.Close()

	err := Push(srv.URL, []Stream{{Labels: map[string]string{"a": "b"}}})
	if err == nil {
		t.Error("Push() = nil error, want error for 400")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd dash-generator && go test ./internal/lokiclient/... -v`
Expected: FAIL — package has no exported symbols yet.

- [ ] **Step 3: Implement lokiclient.go**

`dash-generator/internal/lokiclient/lokiclient.go`:
```go
// Package lokiclient is the shared HTTP client for talking to Loki: instant
// queries, range queries, and pushing new log lines.
package lokiclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Series is one Loki result series: its labels plus its (timestamp,value)
// points. An instant query's single value is normalized into a
// one-element Values slice, so callers share one type with QueryRange.
type Series struct {
	Metric map[string]string
	Values [][2]string
}

type queryResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]string         `json:"value"`
			Values [][2]string       `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func doGet(lokiURL, path string, params url.Values) (*queryResponse, error) {
	u, err := url.Parse(lokiURL)
	if err != nil {
		return nil, fmt.Errorf("invalid loki url %q: %w", lokiURL, err)
	}
	u.Path = path
	u.RawQuery = params.Encode()

	resp, err := http.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("loki %d: %s", resp.StatusCode, truncate(string(body), 160))
	}
	var qr queryResponse
	if err := json.Unmarshal(body, &qr); err != nil {
		return nil, fmt.Errorf("loki response decode: %w", err)
	}
	if qr.Status != "success" {
		return nil, fmt.Errorf("loki status=%s", qr.Status)
	}
	return &qr, nil
}

// Query runs an instant query (Loki's /loki/api/v1/query) at atUnix.
func Query(lokiURL, query string, atUnix int64) ([]Series, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("time", strconv.FormatInt(atUnix, 10))
	qr, err := doGet(lokiURL, "/loki/api/v1/query", params)
	if err != nil {
		return nil, err
	}
	out := make([]Series, 0, len(qr.Data.Result))
	for _, r := range qr.Data.Result {
		out = append(out, Series{Metric: r.Metric, Values: [][2]string{r.Value}})
	}
	return out, nil
}

// QueryRange runs a range query (/loki/api/v1/query_range).
func QueryRange(lokiURL, query string, startUnix, endUnix, stepSeconds int64) ([]Series, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", strconv.FormatInt(startUnix, 10))
	params.Set("end", strconv.FormatInt(endUnix, 10))
	params.Set("step", strconv.FormatInt(stepSeconds, 10))
	qr, err := doGet(lokiURL, "/loki/api/v1/query_range", params)
	if err != nil {
		return nil, err
	}
	out := make([]Series, 0, len(qr.Data.Result))
	for _, r := range qr.Data.Result {
		out = append(out, Series{Metric: r.Metric, Values: r.Values})
	}
	return out, nil
}

// Stream is one Loki push stream: labels plus (timestamp_ns, line, metadata) triples.
type Stream struct {
	Labels map[string]string
	Values []StreamValue
}

// StreamValue is one pushed line: nanosecond timestamp (as a string, Loki's
// own format), the line text, and structured-metadata fields.
type StreamValue struct {
	TimestampNs string
	Line        string
	Metadata    map[string]string
}

type pushBody struct {
	Streams []pushStream `json:"streams"`
}
type pushStream struct {
	Stream map[string]string `json:"stream"`
	Values []pushValue       `json:"values"`
}
type pushValue [3]interface{}

// Push posts to /loki/api/v1/push.
func Push(lokiURL string, streams []Stream) error {
	body := pushBody{Streams: make([]pushStream, 0, len(streams))}
	for _, s := range streams {
		values := make([]pushValue, 0, len(s.Values))
		for _, v := range s.Values {
			values = append(values, pushValue{v.TimestampNs, v.Line, v.Metadata})
		}
		body.Streams = append(body.Streams, pushStream{Stream: s.Labels, Values: values})
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	u, err := url.Parse(lokiURL)
	if err != nil {
		return fmt.Errorf("invalid loki url %q: %w", lokiURL, err)
	}
	u.Path = "/loki/api/v1/push"

	resp, err := http.Post(u.String(), "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("loki push %d: %s", resp.StatusCode, truncate(string(respBody), 160))
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd dash-generator && go test ./internal/lokiclient/... -v`
Expected: PASS (6 tests).

- [ ] **Step 5: Commit**

```bash
git add dash-generator/internal/lokiclient
git commit -m "$(cat <<'EOF'
Add lokiclient package: shared Loki query/push HTTP client

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: `internal/state` — persisted rate-publish dedup state

**Files:**
- Create: `dash-generator/internal/state/state.go`
- Test: `dash-generator/internal/state/state_test.go`

**Interfaces:**
- Produces: `state.State{RatePublished map[string]int64}`, `state.Load(path string) (*State, error)`, `state.Save(path string, st *State) error`.

- [ ] **Step 1: Write the failing tests**

`dash-generator/internal/state/state_test.go`:
```go
package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_MissingFileReturnsZeroValue(t *testing.T) {
	st, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.RatePublished == nil || len(st.RatePublished) != 0 {
		t.Errorf("RatePublished = %v, want empty non-nil map", st.RatePublished)
	}
}

func TestSaveThenLoad_RoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := &State{RatePublished: map[string]int64{"20m:a@example.com": 1700000000}}
	if err := Save(path, st); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RatePublished["20m:a@example.com"] != 1700000000 {
		t.Errorf("loaded = %v", loaded.RatePublished)
	}
}

func TestSave_CreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "dir", "state.json")
	if err := Save(path, &State{RatePublished: map[string]int64{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd dash-generator && go test ./internal/state/... -v`
Expected: FAIL — `Load`/`Save`/`State` undefined.

- [ ] **Step 3: Implement state.go**

`dash-generator/internal/state/state.go`:
```go
// Package state persists dash-generator's small rate-publish dedup
// record between runs.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// State is dash-generator's entire persisted state: the last-published
// timestamp per "halflife:email" key, so a restart never re-publishes or
// duplicates a rate point.
type State struct {
	RatePublished map[string]int64 `json:"ratePublished"`
}

// Load reads path, or returns a zero-value State if it doesn't exist yet.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{RatePublished: map[string]int64{}}, nil
		}
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	if st.RatePublished == nil {
		st.RatePublished = map[string]int64{}
	}
	return &st, nil
}

// Save writes st to path, creating parent directories as needed, via a
// write-temp-then-rename so a kill mid-write never leaves truncated state.
func Save(path string, st *State) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd dash-generator && go test ./internal/state/... -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add dash-generator/internal/state
git commit -m "$(cat <<'EOF'
Add state package: persisted rate-publish dedup record

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: `internal/ratemeter` — port of `rate-meter.mjs`

**Files:**
- Create: `dash-generator/internal/ratemeter/ratemeter.go`
- Test: `dash-generator/internal/ratemeter/ratemeter_test.go`

**Interfaces:**
- Consumes: `lokiclient.{Query,QueryRange,Push,Series,Stream,StreamValue}` (Task 2), `state.State` (Task 3).
- Produces: `ratemeter.PublishConfig{LokiURL, HalfLifeLabel string, HalfLifeS int64, BackfillDays int, DryRun bool}`, `ratemeter.PublishRate(cfg PublishConfig, st *state.State, log func(string, ...any)) (int, error)`.

- [ ] **Step 1: Write the failing tests**

`dash-generator/internal/ratemeter/ratemeter_test.go`:
```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd dash-generator && go test ./internal/ratemeter/... -v`
Expected: FAIL — package has no exported symbols yet.

- [ ] **Step 3: Implement ratemeter.go**

`dash-generator/internal/ratemeter/ratemeter.go`:
```go
// Package ratemeter ports rate-meter.mjs: an exponentially-weighted moving
// average of token consumption per account, published back into Loki so
// the rate panel has inertia instead of a boxcar's cliff-edge jumps.
package ratemeter

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"claude-observability-dash-generator/internal/lokiclient"
	"claude-observability-dash-generator/internal/state"
)

const (
	Stream  = "claude-code-rate"
	BucketS = 300
)

var TokenFields = []string{"input_tokens", "output_tokens", "cache_creation_tokens"}

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

func apiSelector(emailPattern string) string {
	return fmt.Sprintf("{service_name=\"claude-code\"} | event_name = `api_request` | user_email =~ `%s`", emailPattern)
}

// Point is one (unix-seconds timestamp, value) sample.
type Point struct {
	TsS   int64
	Value float64
}

func rateBuckets(lokiURL, emailPattern string, fields []string, startS, endS int64) ([]Point, error) {
	selector := apiSelector(emailPattern)
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = fmt.Sprintf("(sum(sum_over_time(%s | unwrap %s [%ds])) or vector(0))", selector, f, BucketS)
	}
	expr := strings.Join(parts, " + ")

	series, err := lokiclient.QueryRange(lokiURL, expr, startS, endS, BucketS)
	if err != nil {
		return nil, err
	}
	if len(series) == 0 {
		return nil, nil
	}
	perHour := 3600.0 / BucketS
	out := make([]Point, 0, len(series[0].Values))
	for _, v := range series[0].Values {
		ts, err := strconv.ParseFloat(v[0], 64)
		if err != nil {
			continue
		}
		val, err := strconv.ParseFloat(v[1], 64)
		if err != nil {
			continue
		}
		out = append(out, Point{TsS: int64(math.Round(ts)), Value: val * perHour})
	}
	return out, nil
}

// Loki caps a range query's point count, so a long backfill is fetched a
// day at a time and stitched back together in order.
func rateBucketsOver(lokiURL, emailPattern string, fields []string, fromS, toS int64) ([]Point, error) {
	var out []Point
	const day = 24 * 3600
	for start := fromS; start < toS; start += day {
		end := start + day
		if end > toS {
			end = toS
		}
		pts, err := rateBuckets(lokiURL, emailPattern, fields, start, end)
		if err != nil {
			return nil, err
		}
		out = append(out, pts...)
	}
	return out, nil
}

func ewma(points []Point, halfLifeS int64) []Point {
	alpha := 1 - math.Exp(-math.Ln2*BucketS/float64(halfLifeS))
	value := 0.0
	out := make([]Point, len(points))
	for i, p := range points {
		value = alpha*p.Value + (1-alpha)*value
		out[i] = Point{TsS: p.TsS, Value: value}
	}
	return out
}

// PublishConfig configures one PublishRate call.
type PublishConfig struct {
	LokiURL       string
	HalfLifeS     int64
	HalfLifeLabel string // e.g. "20m" — goes into the stream label
	BackfillDays  int
	DryRun        bool
}

// PublishRate computes and pushes the smoothed rate for every account with
// recent activity, using and updating st.RatePublished for dedup. Returns
// the number of points published (or, under DryRun, that would have been).
func PublishRate(cfg PublishConfig, st *state.State, log func(string, ...any)) (int, error) {
	nowS := time.Now().Unix()
	accounts, err := lokiclient.Query(cfg.LokiURL,
		"sum by (user_email) (count_over_time({service_name=\"claude-code\"} | event_name = `api_request` [7d]))",
		nowS)
	if err != nil {
		return 0, err
	}
	var emails []string
	for _, s := range accounts {
		if e := s.Metric["user_email"]; e != "" {
			emails = append(emails, e)
		}
	}
	if len(emails) == 0 {
		return 0, nil
	}

	if st.RatePublished == nil {
		st.RatePublished = map[string]int64{}
	}
	var streams []lokiclient.Stream
	total := 0

	for _, email := range emails {
		key := cfg.HalfLifeLabel + ":" + email
		last := st.RatePublished[key]
		warmup := int64(6 * 3600)
		if cfg.HalfLifeS*8 > warmup {
			warmup = cfg.HalfLifeS * 8
		}
		var from int64
		if last != 0 {
			from = last - warmup
		} else {
			from = nowS - int64(cfg.BackfillDays)*24*3600
		}
		pattern := escapeRegex(email)

		totalPts, err := rateBucketsOver(cfg.LokiURL, pattern, TokenFields, from, nowS)
		if err != nil {
			log("rate for %s not measured this pass: %v", email, err)
			continue
		}
		inputPts, err := rateBucketsOver(cfg.LokiURL, pattern, []string{"input_tokens"}, from, nowS)
		if err != nil {
			log("rate for %s not measured this pass: %v", email, err)
			continue
		}
		outputPts, err := rateBucketsOver(cfg.LokiURL, pattern, []string{"output_tokens"}, from, nowS)
		if err != nil {
			log("rate for %s not measured this pass: %v", email, err)
			continue
		}

		smoothTotal := ewma(totalPts, cfg.HalfLifeS)
		inputByTs := map[int64]float64{}
		for _, p := range ewma(inputPts, cfg.HalfLifeS) {
			inputByTs[p.TsS] = p.Value
		}
		outputByTs := map[int64]float64{}
		for _, p := range ewma(outputPts, cfg.HalfLifeS) {
			outputByTs[p.TsS] = p.Value
		}

		var fresh []Point
		for _, p := range smoothTotal {
			if p.TsS > last {
				fresh = append(fresh, p)
			}
		}
		if len(fresh) == 0 {
			continue
		}

		values := make([]lokiclient.StreamValue, 0, len(fresh))
		for _, p := range fresh {
			values = append(values, lokiclient.StreamValue{
				TimestampNs: fmt.Sprintf("%d000000000", p.TsS),
				Line:        "rate",
				Metadata: map[string]string{
					"rate":        strconv.Itoa(int(math.Round(p.Value))),
					"rate_input":  strconv.Itoa(int(math.Round(inputByTs[p.TsS]))),
					"rate_output": strconv.Itoa(int(math.Round(outputByTs[p.TsS]))),
				},
			})
		}
		streams = append(streams, lokiclient.Stream{
			Labels: map[string]string{"service_name": Stream, "user_email": email, "halflife": cfg.HalfLifeLabel},
			Values: values,
		})
		if !cfg.DryRun {
			st.RatePublished[key] = fresh[len(fresh)-1].TsS
		}
		if last == 0 {
			log("rate: backfilled %d point(s) for %s", len(fresh), email)
		}
		total += len(fresh)
	}

	if len(streams) == 0 {
		return 0, nil
	}
	if cfg.DryRun {
		log("--dry-run: %d rate point(s) NOT published", total)
		return total, nil
	}
	if err := lokiclient.Push(cfg.LokiURL, streams); err != nil {
		log("rate meter did not publish: %v", err)
		return 0, nil
	}
	return total, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd dash-generator && go test ./internal/ratemeter/... -v`
Expected: PASS (5 tests).

- [ ] **Step 5: Commit**

```bash
git add dash-generator/internal/ratemeter
git commit -m "$(cat <<'EOF'
Add ratemeter package: EWMA rate publishing, port of rate-meter.mjs

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: `internal/dashboardgen` — math/string helpers

**Files:**
- Create: `dash-generator/internal/dashboardgen/dashboardgen.go`
- Test: `dash-generator/internal/dashboardgen/dashboardgen_test.go`

**Interfaces:**
- Produces: `dashboardgen.Cutlines{P75, Outlier, Extreme int64, Fallback bool}`, unexported `slug(email string) string`, `escapeRegex(value string) string`, `quantile(sorted []float64, q float64) (float64, bool)`, `commaFormat(n int64) string`, `allPanels(node map[string]interface{}) []map[string]interface{}`, `assertNoPlaceholders(dashboard map[string]interface{}) error`, `replaceVariable(dashboard, variable map[string]interface{})`. These stay unexported (package-internal) — Task 8 exports the one entry point, `GenerateDashboards`.

- [ ] **Step 1: Write the failing tests**

`dash-generator/internal/dashboardgen/dashboardgen_test.go`:
```go
package dashboardgen

import "testing"

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"A@Example.com":  "a-example-com",
		"--x--":           "x",
		"already-clean":   "already-clean",
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
		0:         "0",
		999:       "999",
		1000:      "1,000",
		1234567:   "1,234,567",
		-1234:     "-1,234",
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd dash-generator && go test ./internal/dashboardgen/... -v`
Expected: FAIL — package has no exported symbols yet.

- [ ] **Step 3: Implement the helpers**

`dash-generator/internal/dashboardgen/dashboardgen.go`:
```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd dash-generator && go test ./internal/dashboardgen/... -v`
Expected: PASS (9 tests).

- [ ] **Step 5: Commit**

```bash
git add dash-generator/internal/dashboardgen
git commit -m "$(cat <<'EOF'
Add dashboardgen math/string/JSON-tree helpers, port of dashboard-generator.mjs

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: `internal/dashboardgen` — Loki-backed discovery helpers

**Files:**
- Modify: `dash-generator/internal/dashboardgen/dashboardgen.go`
- Modify: `dash-generator/internal/dashboardgen/dashboardgen_test.go`

**Interfaces:**
- Consumes: `lokiclient.{Query,QueryRange}` (Task 2), `Cutlines` (Task 5).
- Produces: unexported `discoverAccounts(lokiURL string) ([]string, error)`, `Option{Text, Value string}`, `rateCutlines(lokiURL, exporterStream, rateHalfLife, email, field string) Cutlines`, `skillOwners(lokiURL, exporterStream, email string) []Option`, `mcpServers(lokiURL, exporterStream, email string) []string`.

- [ ] **Step 1: Add the failing tests**

Append to `dash-generator/internal/dashboardgen/dashboardgen_test.go`:
```go
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
			// 25 smoothed points, values 1..25, all timestamps "busy".
			var vals []string
			for i := 1; i <= 25; i++ {
				vals = append(vals, fmt.Sprintf(`["%d","%d"]`, 1700000000+i*300, i))
			}
			w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{},"values":[` + strings.Join(vals, ",") + `]}
			]}}`))
		default: // activity query: every bucket has 1 request (busy)
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
```

Add the needed imports (`fmt`, `net/http`, `net/http/httptest`, `strings`, `testing`) to the test file's import block.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd dash-generator && go test ./internal/dashboardgen/... -v`
Expected: FAIL — `discoverAccounts`/`rateCutlines`/`skillOwners`/`mcpServers` undefined.

- [ ] **Step 3: Implement the discovery helpers**

Append to `dash-generator/internal/dashboardgen/dashboardgen.go` (add `sort`, `strconv` — already imported —, `time` to the import block):
```go
import (
	// ... existing imports ...
	"sort"
	"time"

	"claude-observability-dash-generator/internal/lokiclient"
)

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
```

Also add `"math"` to the top-of-file import block (used by `rateCutlines`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd dash-generator && go test ./internal/dashboardgen/... -v`
Expected: PASS (15 tests total).

- [ ] **Step 5: Commit**

```bash
git add dash-generator/internal/dashboardgen
git commit -m "$(cat <<'EOF'
Add dashboardgen Loki discovery helpers: accounts, cutlines, servers, owners

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: `internal/dashboardgen` — `scopeAccount` (template scoping)

**Files:**
- Modify: `dash-generator/internal/dashboardgen/dashboardgen.go`
- Modify: `dash-generator/internal/dashboardgen/dashboardgen_test.go`
- Create: `dash-generator/internal/dashboardgen/testdata/template.json`

**Interfaces:**
- Consumes: `Cutlines`, `Option` (Task 5/6).
- Produces: `AllCutlines{Total, Input, Output Cutlines}`, unexported `accountVariable(email string) map[string]interface{}`, `applyCutlines(dashboard map[string]interface{}, byName map[string]Cutlines)`, `AccountLimits{Block5h, Week int64}`, `loadLimits(path string) (map[string]interface{}, error)`, `limitsFor(doc map[string]interface{}, email string) AccountLimits`, `scopeAccount(template map[string]interface{}, email string, cutlines AllCutlines, servers []string, owners []Option, limits AccountLimits, exporterStream, rateHalfLife string) (map[string]interface{}, error)`.

- [ ] **Step 1: Add the fixture template**

`dash-generator/internal/dashboardgen/testdata/template.json` — a small dashboard with the substitution points `scopeAccount` is documented to touch:
```json
{
  "uid": "TEMPLATE",
  "title": "Claude Code — TEMPLATE",
  "description": "placeholder",
  "templating": { "list": [] },
  "panels": [
    {
      "id": 1,
      "type": "timeseries",
      "fieldConfig": {
        "defaults": {
          "thresholds": { "steps": [ {"value": null}, {"value": 0}, {"value": 0}, {"value": 0} ] },
          "links": [ { "url": "/d/__DASHBOARD__" } ]
        },
        "overrides": [
          {
            "matcher": { "options": "input" },
            "properties": [
              { "id": "thresholds", "value": { "steps": [ {"value": null}, {"value": 0}, {"value": 0} ] } }
            ]
          }
        ]
      },
      "targets": [
        { "refId": "A", "expr": "sum({service_name=\"__EXPORTER_STREAM__\", halflife=\"__HALFLIFE__\"} | rate > __CUT_P75__ and rate < __CUT_OUTLIER__ or rate < __CUT_EXTREME__)" }
      ]
    }
  ]
}
```

- [ ] **Step 2: Add the failing tests**

Append to `dash-generator/internal/dashboardgen/dashboardgen_test.go`:
```go
import (
	// add: "encoding/json", "os", "path/filepath"
)

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
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd dash-generator && go test ./internal/dashboardgen/... -v`
Expected: FAIL — `scopeAccount`/`AllCutlines`/`AccountLimits`/`loadLimits`/`limitsFor` undefined.

- [ ] **Step 4: Implement `scopeAccount` and limits**

Append to `dash-generator/internal/dashboardgen/dashboardgen.go` (add `"encoding/json"`, `"os"` to imports):
```go
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
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd dash-generator && go test ./internal/dashboardgen/... -v`
Expected: PASS (21 tests total).

- [ ] **Step 6: Commit**

```bash
git add dash-generator/internal/dashboardgen
git commit -m "$(cat <<'EOF'
Add scopeAccount: per-account dashboard template scoping

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: `internal/dashboardgen` — `GenerateDashboards` orchestration

**Files:**
- Modify: `dash-generator/internal/dashboardgen/dashboardgen.go`
- Modify: `dash-generator/internal/dashboardgen/dashboardgen_test.go`

**Interfaces:**
- Consumes: everything from Tasks 5-7.
- Produces: `dashboardgen.Config{LokiURL, ExporterStream, RateHalfLife, GrafanaDir string}`, `dashboardgen.GenerateDashboards(cfg Config, log func(string, ...any)) error` — the package's one exported entry point.

- [ ] **Step 1: Add the failing end-to-end test**

Append to `dash-generator/internal/dashboardgen/dashboardgen_test.go`:
```go
func TestGenerateDashboards_WritesOneFilePerAccountAndRemovesStale(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		switch {
		case strings.Contains(query, "user_email") && !strings.Contains(query, "claude-code-rate"):
			// discoverAccounts, and the busy/activity query inside rateCutlines
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"user_email":"a@example.com"}}
			]}}`))
		default:
			w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
		}
	}))
	defer srv.Close()

	grafanaDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(grafanaDir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	template, err := os.ReadFile(filepath.Join("testdata", "template.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grafanaDir, "templates", "claude-code.json"), template, 0o644); err != nil {
		t.Fatal(err)
	}
	// A stale dashboard for an account no longer in Loki — must be removed.
	if err := os.MkdirAll(filepath.Join(grafanaDir, "dashboards", "accounts"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(grafanaDir, "dashboards", "accounts", "stale-example-com.json")
	if err := os.WriteFile(stale, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = GenerateDashboards(Config{
		LokiURL: srv.URL, ExporterStream: "claude-code-exporter-1", RateHalfLife: "20m", GrafanaDir: grafanaDir,
	}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale dashboard was not removed")
	}
	generated := filepath.Join(grafanaDir, "dashboards", "accounts", "a-example-com.json")
	if _, err := os.Stat(generated); err != nil {
		t.Errorf("expected dashboard not written: %v", err)
	}
}

func TestGenerateDashboards_SkipsIgnoredAccounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		if strings.Contains(query, "user_email") && !strings.Contains(query, "claude-code-rate") {
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"user_email":"ignored@example.com"}}
			]}}`))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer srv.Close()

	grafanaDir := t.TempDir()
	os.MkdirAll(filepath.Join(grafanaDir, "templates"), 0o755)
	template, _ := os.ReadFile(filepath.Join("testdata", "template.json"))
	os.WriteFile(filepath.Join(grafanaDir, "templates", "claude-code.json"), template, 0o644)
	os.WriteFile(filepath.Join(grafanaDir, "account-limits.json"), []byte(`{"ignore":["ignored@example.com"]}`), 0o644)

	if err := GenerateDashboards(Config{LokiURL: srv.URL, ExporterStream: "s", RateHalfLife: "20m", GrafanaDir: grafanaDir}, t.Logf); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(filepath.Join(grafanaDir, "dashboards", "accounts"))
	if len(entries) != 0 {
		t.Errorf("got %d dashboards, want 0 (account is ignored)", len(entries))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd dash-generator && go test ./internal/dashboardgen/... -v`
Expected: FAIL — `Config`/`GenerateDashboards` undefined.

- [ ] **Step 3: Implement `GenerateDashboards`**

Append to `dash-generator/internal/dashboardgen/dashboardgen.go` (add `"path/filepath"` to imports):
```go
// Config configures one GenerateDashboards pass.
type Config struct {
	LokiURL        string
	ExporterStream string
	RateHalfLife   string
	GrafanaDir     string // .../grafana — templates/, account-limits.json, dashboards/accounts/
}

// GenerateDashboards discovers every account with data, skips ones on the
// ignore list, computes cutlines/servers/owners per account, scopes the
// template, and writes/removes files under GrafanaDir/dashboards/accounts/
// so the directory exactly matches accounts that currently have data.
func GenerateDashboards(cfg Config, log func(string, ...any)) error {
	templatePath := filepath.Join(cfg.GrafanaDir, "templates", "claude-code.json")
	limitsPath := filepath.Join(cfg.GrafanaDir, "account-limits.json")
	outDir := filepath.Join(cfg.GrafanaDir, "dashboards", "accounts")

	templateData, err := os.ReadFile(templatePath)
	if err != nil {
		return err
	}
	var template map[string]interface{}
	if err := json.Unmarshal(templateData, &template); err != nil {
		return err
	}

	limits, err := loadLimits(limitsPath)
	if err != nil {
		return err
	}
	ignoreList, _ := limits["ignore"].([]interface{})
	ignore := map[string]bool{}
	for _, e := range ignoreList {
		if s, ok := e.(string); ok {
			ignore[s] = true
		}
	}

	allEmails, err := discoverAccounts(cfg.LokiURL)
	if err != nil {
		return err
	}
	var emails []string
	for _, e := range allEmails {
		if ignore[e] {
			log("ignored: %s ('ignore' list in account-limits.json)", e)
			continue
		}
		emails = append(emails, e)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	wanted := map[string]map[string]interface{}{}
	for _, email := range emails {
		total := rateCutlines(cfg.LokiURL, cfg.ExporterStream, cfg.RateHalfLife, email, "rate")
		input := rateCutlines(cfg.LokiURL, cfg.ExporterStream, cfg.RateHalfLife, email, "rate_input")
		output := rateCutlines(cfg.LokiURL, cfg.ExporterStream, cfg.RateHalfLife, email, "rate_output")
		servers := mcpServers(cfg.LokiURL, cfg.ExporterStream, email)
		owners := skillOwners(cfg.LokiURL, cfg.ExporterStream, email)
		accountLimits := limitsFor(limits, email)

		dashboard, err := scopeAccount(template, email, AllCutlines{total, input, output}, servers, owners,
			accountLimits, cfg.ExporterStream, cfg.RateHalfLife)
		if err != nil {
			return err
		}
		wanted[slug(email)+".json"] = dashboard

		log("%s  ->  total cutlines P75 %s / outlier %s tokens/h / extreme %s, %d MCP server(s), %d skill owner(s), limits %s/5h and %s/week",
			email, commaFormat(total.P75), commaFormat(total.Outlier), commaFormat(total.Extreme),
			len(servers), len(owners), commaFormat(accountLimits.Block5h), commaFormat(accountLimits.Week))
	}

	existing, _ := os.ReadDir(outDir)
	for _, e := range existing {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if _, ok := wanted[name]; !ok {
			os.Remove(filepath.Join(outDir, name))
			log("removed: %s (account has no data)", name)
		}
	}

	for file, dashboard := range wanted {
		out, err := json.MarshalIndent(dashboard, "", "  ")
		if err != nil {
			return err
		}
		out = append(out, '\n')
		target := filepath.Join(outDir, file)
		tmp := fmt.Sprintf("%s.tmp-%d", target, os.Getpid())
		if err := os.WriteFile(tmp, out, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, target); err != nil {
			return err
		}
		log("  %v  ->  %s", dashboard["uid"], file)
	}

	log("%d dashboard(s), one per account, in %s", len(wanted), outDir)
	if len(emails) == 0 {
		if len(allEmails) > 0 {
			log("No dashboard generated: all %d account(s) with data are on the 'ignore' list.", len(allEmails))
		} else {
			log("No account yet — run a Claude session and try again.")
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd dash-generator && go test ./internal/dashboardgen/... -v`
Expected: PASS (23 tests total).

- [ ] **Step 5: Commit**

```bash
git add dash-generator/internal/dashboardgen
git commit -m "$(cat <<'EOF'
Add GenerateDashboards orchestration, completing the dashboardgen port

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 9: `cmd/dash-generator/main.go` — wire the two loops

**Files:**
- Modify: `dash-generator/cmd/dash-generator/main.go`

**Interfaces:**
- Consumes: `state.{Load,Save,State}` (Task 3), `ratemeter.{PublishConfig,PublishRate}` (Task 4), `dashboardgen.{Config,GenerateDashboards}` (Task 8).
- Produces: the `dash-generator` binary's end-to-end behavior.

- [ ] **Step 1: Replace main.go with the full orchestration**

`dash-generator/cmd/dash-generator/main.go`:
```go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"claude-observability-dash-generator/internal/dashboardgen"
	"claude-observability-dash-generator/internal/ratemeter"
	"claude-observability-dash-generator/internal/state"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	repoRoot, err := findRepoRoot()
	if err != nil {
		return err
	}

	lokiURL := envOr("LOKI_URL", "http://localhost:47100")
	exporterStream := envOr("EXPORTER_STREAM", "claude-code-exporter-1")
	rateHalfLife := envOr("RATE_HALFLIFE", "20m")
	halfLifeS, err := parseHalfLife(rateHalfLife)
	if err != nil {
		return err
	}
	backfillDays := envInt("RATE_BACKFILL_DAYS", 14)
	pollSeconds := envInt("POLL_SECONDS", 60)
	dashboardIntervalSeconds := envInt("DASHBOARD_INTERVAL_SECONDS", 600)
	statePath := filepath.Join(repoRoot, ".state", "dash-generator-state.json")

	once := hasArg("--once")
	dryRun := hasArg("--dry-run")

	st, err := state.Load(statePath)
	if err != nil {
		return err
	}

	log := func(format string, args ...any) { fmt.Printf(time.Now().UTC().Format(time.RFC3339)+" "+format+"\n", args...) }

	ratePass := func() error {
		n, err := ratemeter.PublishRate(ratemeter.PublishConfig{
			LokiURL: lokiURL, HalfLifeS: halfLifeS, HalfLifeLabel: rateHalfLife,
			BackfillDays: backfillDays, DryRun: dryRun,
		}, st, log)
		if err != nil {
			log("rate meter failed: %v", err)
			return nil
		}
		if n > 0 {
			log("%d rate point(s) published", n)
		}
		if !dryRun {
			return state.Save(statePath, st)
		}
		return nil
	}

	dashboardPass := func() error {
		if dryRun {
			return nil
		}
		if err := dashboardgen.GenerateDashboards(dashboardgen.Config{
			LokiURL: lokiURL, ExporterStream: exporterStream, RateHalfLife: rateHalfLife, GrafanaDir: filepath.Join(repoRoot, "grafana"),
		}, log); err != nil {
			log("dashboard generation failed: %v", err)
		}
		return nil
	}

	if once {
		if err := ratePass(); err != nil {
			return err
		}
		return dashboardPass()
	}

	errCh := make(chan error, 2)
	go func() { errCh <- loop(pollSeconds, ratePass) }()
	go func() { errCh <- loop(dashboardIntervalSeconds, dashboardPass) }()
	return <-errCh
}

func loop(intervalSeconds int, run func() error) error {
	for {
		if err := run(); err != nil {
			return err
		}
		time.Sleep(time.Duration(intervalSeconds) * time.Second)
	}
}

// findRepoRoot requires the binary to be run from the claude-observability
// repo root — it doesn't bundle grafana/templates or docker-compose.yaml,
// so those still have to come from the checkout it's invoked inside.
func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(wd, "docker-compose.yaml")); err != nil {
		return "", fmt.Errorf("must be run from the claude-observability repo root (docker-compose.yaml not found in %s)", wd)
	}
	return wd, nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func hasArg(name string) bool {
	for _, a := range os.Args[1:] {
		if a == name {
			return true
		}
	}
	return false
}

var halfLifePattern = regexp.MustCompile(`^(\d+)([smh])$`)

func parseHalfLife(value string) (int64, error) {
	m := halfLifePattern.FindStringSubmatch(value)
	if m == nil {
		return 0, fmt.Errorf("RATE_HALFLIFE must look like 20m, 90s or 2h (got %q)", value)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	switch m[2] {
	case "s":
		return n, nil
	case "m":
		return n * 60, nil
	case "h":
		return n * 3600, nil
	}
	return 0, fmt.Errorf("unreachable")
}
```

- [ ] **Step 2: Verify it builds**

Run: `cd dash-generator && go build ./...`
Expected: no errors.

- [ ] **Step 3: Manual smoke test — `--once` against a fake Loki**

From a scratch directory (NOT the real repo — this avoids any risk of writing into real `grafana/` state):
```bash
mkdir -p /tmp/dash-generator-smoke/grafana/templates
mkdir -p /tmp/dash-generator-smoke/.state
touch /tmp/dash-generator-smoke/docker-compose.yaml
cp dash-generator/internal/dashboardgen/testdata/template.json /tmp/dash-generator-smoke/grafana/templates/claude-code.json
cd dash-generator && go build -o /tmp/dash-generator-smoke/dash-generator ./cmd/dash-generator
python3 -c "
import http.server, json
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(json.dumps({'status':'success','data':{'resultType':'vector','result':[]}}).encode())
http.server.HTTPServer(('127.0.0.1', 47100), H).serve_forever()
" &
SERVER_PID=$!
sleep 1
cd /tmp/dash-generator-smoke && LOKI_URL=http://127.0.0.1:47100 ./dash-generator --once
kill $SERVER_PID
```
Expected: prints `No account yet — run a Claude session and try again.`, exits 0, no error.

- [ ] **Step 4: Commit**

```bash
git add dash-generator/cmd/dash-generator/main.go
git commit -m "$(cat <<'EOF'
Wire dash-generator's two loops (rate publish, dashboard generation) in main.go

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 10: Release workflow

**Files:**
- Modify: `.github/workflows/release.yml`

**Interfaces:**
- Produces: on pushing a tag matching `v*`, adds a `dash-generator` build job (alongside the existing `setup` job) producing `dash-generator-<goos>-<goarch>[.exe]`, uploaded to the same GitHub Release the `setup` binaries go to.

One shared `release.yml` for all three binaries, not a file per binary — matches the `test.yml` consolidation from Task 1. This task adds the `dash-generator` build job and lists it in the final `release` job's `needs:`.

- [ ] **Step 1: Add the `dash-generator` build job**

In `.github/workflows/release.yml`, add (alongside the existing `setup` job):
```yaml
  dash-generator:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include:
          - goos: darwin
            goarch: amd64
          - goos: darwin
            goarch: arm64
          - goos: linux
            goarch: amd64
          - goos: linux
            goarch: arm64
          - goos: windows
            goarch: amd64
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - name: build
        working-directory: dash-generator
        env:
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
          CGO_ENABLED: '0'
        run: |
          ext=""
          if [ "${{ matrix.goos }}" = "windows" ]; then ext=".exe"; fi
          go build -o "dash-generator-${{ matrix.goos }}-${{ matrix.goarch }}${ext}" ./cmd/dash-generator
      - uses: actions/upload-artifact@v4
        with:
          name: binaries-dash-generator-${{ matrix.goos }}-${{ matrix.goarch }}
          path: dash-generator/dash-generator-*
```
Update the `release` job's `needs:` to include it: `needs: [setup, dash-generator]` (becomes `[setup, dash-generator, collector]` once the collector plan adds its own job).

- [ ] **Step 2: Validate the workflow YAML parses**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml'))"`

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "$(cat <<'EOF'
Add dash-generator build job to the shared release workflow

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 11: Generalize `setup/internal/service` for any executable

**Files:**
- Modify: `setup/internal/service/service.go`
- Modify: `setup/internal/service/service_darwin.go`
- Modify: `setup/internal/service/service_darwin_test.go`
- Modify: `setup/internal/service/service_linux.go`
- Modify: `setup/internal/service/service_linux_test.go`
- Modify: `setup/internal/service/service_windows.go`
- Modify: `setup/internal/service/service_windows_test.go`

**Interfaces:**
- Produces: `service.Config{Label, Command string, Args []string, WorkingDir string, Env []envwriter.Var, LogDir string}` (was `{RepoRoot, NodeBin, ClaudeBin string, Env []envwriter.Var, LogDir string}`), `service.GeneratePlist/GenerateUnit/GenerateTaskArgs(cfg Config) ...`, `service.Install(cfg Config) error` — same function names, generalized signature.

This is a refactor of already-shipped code (the wizard's Tasks 7-9), needed because both `dash-generator` (this plan, Task 12) and the future `collector` binary must install themselves as a service without `NodeBin`/`ClaudeBin`/a hardcoded `collector.mjs` path baked into the package.

- [ ] **Step 1: Update the failing tests first**

Replace the `Config` literals in `setup/internal/service/service_darwin_test.go`:
```go
func TestGeneratePlist_ContainsLabelAndEnv(t *testing.T) {
	cfg := Config{
		Label:      "com.agents-observability.dash-generator",
		Command:    "/usr/local/bin/dash-generator",
		WorkingDir: "/repo",
		Env:        []envwriter.Var{{Name: "CLAUDE_DIR", Value: "/home/.claude"}},
		LogDir:     "/repo/.state",
	}
	out := GeneratePlist(cfg)

	for _, want := range []string{
		"com.agents-observability.dash-generator",
		"/usr/local/bin/dash-generator",
		"<key>CLAUDE_DIR</key><string>/home/.claude</string>",
		"/repo/.state/dash-generator.log",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %q", want)
		}
	}
}

func TestGeneratePlist_EscapesXML(t *testing.T) {
	cfg := Config{
		Label: "com.agents-observability.x", Command: "/bin/x", WorkingDir: "/repo", LogDir: "/repo/.state",
		Env: []envwriter.Var{{Name: "FOO", Value: "a & b < c"}},
	}
	out := GeneratePlist(cfg)
	if !strings.Contains(out, "a &amp; b &lt; c") {
		t.Errorf("plist did not escape XML special chars: %s", out)
	}
}

func TestGeneratePlist_IncludesArgs(t *testing.T) {
	cfg := Config{
		Label: "com.agents-observability.x", Command: "/bin/x", Args: []string{"--once"}, WorkingDir: "/repo", LogDir: "/repo/.state",
	}
	out := GeneratePlist(cfg)
	if !strings.Contains(out, "<string>--once</string>") {
		t.Errorf("plist missing arg: %s", out)
	}
}
```

Replace `setup/internal/service/service_linux_test.go`:
```go
func TestGenerateUnit_ContainsExecAndEnv(t *testing.T) {
	cfg := Config{
		Label: "dash-generator", Command: "/usr/local/bin/dash-generator", Args: []string{"--once"},
		WorkingDir: "/repo", LogDir: "/repo/.state",
		Env: []envwriter.Var{{Name: "CLAUDE_DIR", Value: "/home/.claude"}},
	}
	out := GenerateUnit(cfg)

	for _, want := range []string{
		"ExecStart=/usr/local/bin/dash-generator --once",
		"Environment=CLAUDE_DIR=/home/.claude",
		"WantedBy=default.target",
		"StandardOutput=append:/repo/.state/dash-generator.log",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("unit missing %q, got:\n%s", want, out)
		}
	}
}
```

Replace `setup/internal/service/service_windows_test.go`:
```go
func TestGenerateTaskArgs_ContainsTaskNameAndCommand(t *testing.T) {
	cfg := Config{
		Label: "DashGenerator", Command: `C:\bin\dash-generator.exe`, WorkingDir: `C:\repo`, LogDir: `C:\repo\.state`,
		Env: []envwriter.Var{{Name: "CLAUDE_DIR", Value: `C:\Users\me\.claude`}},
	}
	args := GenerateTaskArgs(cfg)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"DashGenerator",
		"onlogon",
		`set CLAUDE_DIR=C:\Users\me\.claude`,
		`C:\bin\dash-generator.exe`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q, got: %s", want, joined)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd setup && go build ./... 2>&1 | head -30`
Expected: compile errors — `Config` no longer has `RepoRoot`/`NodeBin`/`ClaudeBin`/`CollectorScript` fields the old implementation referenced (this task changes both test AND implementation together since they're tightly coupled; verify the OLD implementation fails against the NEW test literals first via `go vet ./internal/service/...`).

- [ ] **Step 3: Rewrite `service.go`**

`setup/internal/service/service.go`:
```go
// Package service installs an executable as a background process: a
// launchd LaunchAgent on macOS, a systemd --user unit on Linux, and a
// logon-triggered Scheduled Task on Windows.
package service

import "claude-observability-setup/internal/envwriter"

// Config holds everything an OS-specific installer needs to register a
// program as a background service. Generic over what it runs — the same
// package installs the wizard-built dash-generator binary today and the
// future collector binary later, with no per-program logic in here.
type Config struct {
	Label      string          // service identifier: reverse-DNS on macOS, unit/task name elsewhere
	Command    string          // absolute path to the executable to run
	Args       []string
	WorkingDir string
	Env        []envwriter.Var
	LogDir     string // absolute path for stdout/stderr logs
}
```

- [ ] **Step 4: Rewrite `service_darwin.go`**

`setup/internal/service/service_darwin.go`:
```go
//go:build darwin

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func plistPath(label string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// GeneratePlist renders the launchd LaunchAgent plist for cfg.
func GeneratePlist(cfg Config) string {
	var envXML strings.Builder
	for _, v := range cfg.Env {
		fmt.Fprintf(&envXML, "    <key>%s</key><string>%s</string>\n", v.Name, xmlEscape(v.Value))
	}
	var argsXML strings.Builder
	fmt.Fprintf(&argsXML, "    <string>%s</string>\n", cfg.Command)
	for _, a := range cfg.Args {
		fmt.Fprintf(&argsXML, "    <string>%s</string>\n", xmlEscape(a))
	}
	pathEnv := filepath.Dir(cfg.Command) + ":/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>WorkingDirectory</key><string>%s</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>%s</string>
%s  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
  <key>ProcessType</key><string>Background</string>
</dict>
</plist>
`, cfg.Label, argsXML.String(), cfg.WorkingDir, pathEnv, envXML.String(),
		filepath.Join(cfg.LogDir, baseName(cfg.Command)+".log"), filepath.Join(cfg.LogDir, baseName(cfg.Command)+".err.log"))
}

func baseName(command string) string {
	return strings.TrimSuffix(filepath.Base(command), filepath.Ext(command))
}

// Install writes the plist and (re)loads it via launchctl.
func Install(cfg Config) error {
	path, err := plistPath(cfg.Label)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(GeneratePlist(cfg)), 0o644); err != nil {
		return err
	}

	uid := fmt.Sprintf("gui/%d", os.Getuid())
	exec.Command("launchctl", "bootout", uid, path).Run()
	if out, err := exec.Command("launchctl", "bootstrap", uid, path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, out)
	}
	if out, err := exec.Command("launchctl", "enable", uid+"/"+cfg.Label).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl enable: %w: %s", err, out)
	}
	return nil
}
```

Note: `TestGeneratePlist_ContainsLabelAndEnv` expects the log file to be
named `dash-generator.log` — verify `baseName("/usr/local/bin/dash-generator")` produces `dash-generator` (no extension to strip on this input, so `filepath.Ext` returns `""` and `TrimSuffix` is a no-op — correct).

- [ ] **Step 5: Rewrite `service_linux.go`**

`setup/internal/service/service_linux.go`:
```go
//go:build linux

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func unitPath(label string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	name := label
	if !strings.HasSuffix(name, ".service") {
		name += ".service"
	}
	return filepath.Join(home, ".config", "systemd", "user", name), nil
}

// GenerateUnit renders the systemd --user unit file for cfg.
func GenerateUnit(cfg Config) string {
	var envLines strings.Builder
	for _, v := range cfg.Env {
		fmt.Fprintf(&envLines, "Environment=%s=%s\n", v.Name, v.Value)
	}
	pathEnv := filepath.Dir(cfg.Command) + ":/usr/local/bin:/usr/bin:/bin"
	execLine := cfg.Command
	for _, a := range cfg.Args {
		execLine += " " + a
	}
	base := strings.TrimSuffix(filepath.Base(cfg.Command), filepath.Ext(cfg.Command))

	return fmt.Sprintf(`[Unit]
Description=%s

[Service]
Type=simple
WorkingDirectory=%s
Environment=PATH=%s
%sExecStart=%s
Restart=always
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, cfg.Label, cfg.WorkingDir, pathEnv, envLines.String(), execLine,
		filepath.Join(cfg.LogDir, base+".log"), filepath.Join(cfg.LogDir, base+".err.log"))
}

// Install writes the unit file and enables + starts it via systemctl --user.
func Install(cfg Config) error {
	path, err := unitPath(cfg.Label)
	if err != nil {
		return err
	}
	unitName := filepath.Base(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(GenerateUnit(cfg)), 0o644); err != nil {
		return err
	}

	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, out)
	}
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable: %w: %s", err, out)
	}
	return nil
}
```

- [ ] **Step 6: Rewrite `service_windows.go`**

`setup/internal/service/service_windows.go`:
```go
//go:build windows

package service

import (
	"fmt"
	"os"
	"os/exec"
)

// GenerateTaskArgs returns the `schtasks /create` argument list that
// registers cfg to run at logon.
func GenerateTaskArgs(cfg Config) []string {
	cmd := ""
	for _, v := range cfg.Env {
		cmd += fmt.Sprintf("set %s=%s&& ", v.Name, v.Value)
	}
	cmd += fmt.Sprintf(`"%s"`, cfg.Command)
	for _, a := range cfg.Args {
		cmd += fmt.Sprintf(` "%s"`, a)
	}

	return []string{
		"/create", "/f",
		"/tn", cfg.Label,
		"/sc", "onlogon",
		"/tr", "cmd /c " + cmd,
	}
}

// Install registers the Scheduled Task via schtasks.
func Install(cfg Config) error {
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return err
	}
	out, err := exec.Command("schtasks", GenerateTaskArgs(cfg)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks /create: %w: %s", err, out)
	}
	return nil
}
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `cd setup && go build ./... && go test ./...`
Expected: PASS, all packages (the `discovery`/`health`/`envwriter`/`limits`/`wizard` packages are untouched by this task and must still be green).

- [ ] **Step 8: Commit**

```bash
git add setup/internal/service
git commit -m "$(cat <<'EOF'
Generalize service.Config to any executable, not just collector.mjs

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 12: Repoint the wizard's service-install step at `dash-generator`

**Files:**
- Modify: `setup/cmd/setup/main.go`

**Interfaces:**
- Consumes: the generalized `service.Config`/`service.Install` (Task 11).

- [ ] **Step 1: Update `installService` and its call site**

In `setup/cmd/setup/main.go`, replace the `installService` function and the "Collector service" section:

```go
func installService(repoRoot string, collectorVars []envwriter.Var) error {
	dashGeneratorBin, err := findDashGeneratorBinary(repoRoot)
	if err != nil {
		return err
	}
	cfg := service.Config{
		Label:      dashGeneratorLabel(),
		Command:    dashGeneratorBin,
		WorkingDir: repoRoot,
		Env:        collectorVars,
		LogDir:     filepath.Join(repoRoot, ".state"),
	}
	return service.Install(cfg)
}

// findDashGeneratorBinary looks for a dash-generator binary built
// alongside this one (same directory as the running setup executable) or
// on PATH — it isn't bundled inside claude-observability-setup itself.
func findDashGeneratorBinary(repoRoot string) (string, error) {
	name := "dash-generator"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), name)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, nil
		}
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("%s not found next to this binary or on PATH — build it from dash-generator/ or download it alongside claude-observability-setup", name)
}

func dashGeneratorLabel() string {
	switch runtime.GOOS {
	case "darwin":
		return "com.agents-observability.dash-generator"
	case "windows":
		return "ClaudeObservabilityDashGenerator"
	default:
		return "claude-observability-dash-generator"
	}
}
```

And change the prompt text in `run()`:
```go
	fmt.Fprintln(out, "\n── Dash generator ─────────────────────────────────────────")
	fmt.Fprintln(out, "  Generates per-account Grafana dashboards and publishes the rate panel —")
	fmt.Fprintln(out, "  transcript scanning / usage-truth aren't wired up to this wizard yet.")
	if wizard.AskYesNo(out, stdin, "  Install the dash generator as a background service now?", true) {
		if err := installService(repoRoot, collectorVars); err != nil {
			return fmt.Errorf("installing dash generator service: %w", err)
		}
		fmt.Fprintln(out, "  installed and started")
	}
```

(This replaces the previous "── Collector service ──" block and its call to the old `installService`.) Add `"runtime"` to the import block if not already present (it already is, from `writeShellConfig`).

- [ ] **Step 2: Verify it builds**

Run: `cd setup && go build ./...`
Expected: no errors.

- [ ] **Step 3: Run the full wizard test suite**

Run: `cd setup && go test ./...`
Expected: PASS, all packages green (the wizard/discovery/health/envwriter/limits packages are unaffected by this task).

- [ ] **Step 4: Manual smoke test — service-install prompt declined**

From a scratch directory with a fake `docker-compose.yaml` and a listening fake endpoint (same pattern as the wizard plan's Task 10 smoke test — NOT the real repo, NOT real `$HOME`):
```bash
mkdir -p /tmp/setup-smoke2 && cd /tmp/setup-smoke2
touch docker-compose.yaml
python3 -m http.server 47317 --bind 127.0.0.1 &
SERVER_PID=$!
HOME=/tmp/setup-smoke2/fakehome printf 'n\n\n\nn\n' | /path/to/claude-observability-setup
kill $SERVER_PID
```
Expected: reaches the "Dash generator" section, prints the new prompt text, and since the answer is "n", does not attempt to find/install a `dash-generator` binary at all.

- [ ] **Step 5: Commit**

```bash
git add setup/cmd/setup/main.go
git commit -m "$(cat <<'EOF'
Repoint the wizard's service-install step at dash-generator

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```
