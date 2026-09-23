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
