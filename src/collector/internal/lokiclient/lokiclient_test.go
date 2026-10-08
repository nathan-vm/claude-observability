package lokiclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const secretPW = "s3cr3t-pw"

func TestParseURL_ErrorOmitsRawURL(t *testing.T) {
	for _, raw := range []string{
		"http://user:" + secretPW + "@ho st:3100",
		"http://user:" + secretPW + "@host:abc",
		"http://user:" + secretPW + "@host:3100/%zz",
	} {
		_, err := ParseURL(raw)
		if err == nil {
			t.Fatalf("ParseURL(%q) = nil error, want error", raw)
		}
		if strings.Contains(err.Error(), secretPW) {
			t.Errorf("ParseURL(%q) error leaks password: %v", raw, err)
		}
	}
}

func TestParseURL_Valid(t *testing.T) {
	u, err := ParseURL("http://user:" + secretPW + "@host:3100")
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "host:3100" || u.User.Username() != "user" {
		t.Errorf("got %v", u)
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct{ raw, want string }{
		{"http://user:" + secretPW + "@host:3100", "http://user:xxxxx@host:3100"},
		{"http://host:3100", "http://host:3100"},
		{"http://user:" + secretPW + "@ho st", "<unparseable url>"},
	}
	for _, tt := range tests {
		got := RedactURL(tt.raw)
		if got != tt.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tt.raw, got, tt.want)
		}
		if strings.Contains(got, secretPW) {
			t.Errorf("RedactURL(%q) leaks password: %q", tt.raw, got)
		}
	}
}

func TestErrorsNeverLeakPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closed := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()

	for _, base := range []string{
		"http://user:" + secretPW + "@" + closed,
		"http://user:" + secretPW + "@ho st:3100",
	} {
		_, qerr := QueryRange(base, "up", 1, 2, 0, "")
		perr := Push(base, nil)
		for name, err := range map[string]error{"QueryRange": qerr, "Push": perr} {
			if err == nil {
				t.Errorf("%s(%q) = nil error, want error", name, base)
				continue
			}
			if strings.Contains(err.Error(), secretPW) {
				t.Errorf("%s(%q) error leaks password: %v", name, base, err)
			}
		}
	}
}

func TestQueryRange_ParsesStreamsResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("limit") != "1" || q.Get("direction") != "backward" {
			t.Errorf("params = %v", q)
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"user_email":"a@example.com","session_id":"s1"},"values":[["1700000000000000000","line text"]]}
		]}}`))
	}))
	defer srv.Close()

	results, err := QueryRange(srv.URL, `{service_name="claude-code"}`, 1, 2, 1, "backward")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Labels["user_email"] != "a@example.com" {
		t.Fatalf("got %+v", results)
	}
	if results[0].Values[0][1] != "line text" {
		t.Errorf("values = %v", results[0].Values)
	}
}

func TestQueryRange_OmitsLimitAndDirectionWhenUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Has("limit") || q.Has("direction") {
			t.Errorf("expected no limit/direction params, got %v", q)
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	if _, err := QueryRange(srv.URL, "up", 1, 2, 0, ""); err != nil {
		t.Fatal(err)
	}
}

func TestQueryRange_NonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"error"}`))
	}))
	defer srv.Close()

	if _, err := QueryRange(srv.URL, "up", 1, 2, 0, ""); err == nil {
		t.Error("want error for status=error")
	}
}

func TestQueryRange_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := QueryRange(srv.URL, "up", 1, 2, 0, ""); err == nil {
		t.Error("want error for 500")
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
		Values: []StreamValue{{TimestampNs: "123000000", Line: "Bash", Metadata: map[string]string{"tool_use_id": "t1"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	streams, _ := received["streams"].([]interface{})
	if len(streams) != 1 {
		t.Fatalf("received = %v", received)
	}
}

func TestPush_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad"))
	}))
	defer srv.Close()

	if err := Push(srv.URL, []Stream{{Labels: map[string]string{"a": "b"}}}); err == nil {
		t.Error("want error for 400")
	}
}
