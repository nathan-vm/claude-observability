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

// point decodes a Prometheus-style [timestamp, value] pair. Loki's metric
// (vector/matrix) results — what Query/QueryRange here always ask for —
// return the timestamp as a raw JSON NUMBER, not a quoted string (unlike
// Loki's own log-query "streams" API, which quotes it). Confirmed against
// a real Loki instance: `[2]string` alone fails to decode
// `"value":[1790207206,"1"]`. Normalized to a plain [2]string either way,
// so callers never have to care which form arrived.
type point [2]string

func (p *point) UnmarshalJSON(data []byte) error {
	var raw [2]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for i, r := range raw {
		var s string
		if err := json.Unmarshal(r, &s); err == nil {
			p[i] = s
			continue
		}
		// Not a JSON string (no surrounding quotes) — a bare number, so
		// its raw text IS its decimal representation already.
		p[i] = string(r)
	}
	return nil
}

type queryResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  point             `json:"value"`
			Values []point           `json:"values"`
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
		out = append(out, Series{Metric: r.Metric, Values: [][2]string{[2]string(r.Value)}})
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
		values := make([][2]string, len(r.Values))
		for i, v := range r.Values {
			values[i] = [2]string(v)
		}
		out = append(out, Series{Metric: r.Metric, Values: values})
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
