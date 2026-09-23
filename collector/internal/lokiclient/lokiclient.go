// Package lokiclient talks to Loki for the collector: log-line range
// queries (Loki's "streams" result type) and pushing new log lines. Not
// the same shape as dash-generator's client — see the spec correction in
// docs/superpowers/specs/2026-09-23-collector-design.md.
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

// StreamResult is one Loki log-query result group: its labels (including
// any structured-metadata fields Loki surfaces here) plus its
// (timestamp_ns, line) pairs.
type StreamResult struct {
	Labels map[string]string
	Values [][2]string
}

type queryStreamsResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Stream map[string]string `json:"stream"`
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

// QueryRange runs a log-query range request. start/end are nanosecond
// Unix timestamps. limit <= 0 omits the "limit" param; direction == ""
// omits "direction".
func QueryRange(lokiURL, query string, startNs, endNs int64, limit int, direction string) ([]StreamResult, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", strconv.FormatInt(startNs, 10))
	params.Set("end", strconv.FormatInt(endNs, 10))
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if direction != "" {
		params.Set("direction", direction)
	}
	u, err := url.Parse(lokiURL)
	if err != nil {
		return nil, fmt.Errorf("invalid loki url %q: %w", lokiURL, err)
	}
	u.Path = "/loki/api/v1/query_range"
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
		return nil, fmt.Errorf("loki %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var qr queryStreamsResponse
	if err := json.Unmarshal(body, &qr); err != nil {
		return nil, fmt.Errorf("loki response decode: %w", err)
	}
	if qr.Status != "success" {
		return nil, fmt.Errorf("loki status=%s", qr.Status)
	}
	out := make([]StreamResult, 0, len(qr.Data.Result))
	for _, r := range qr.Data.Result {
		out = append(out, StreamResult{Labels: r.Stream, Values: r.Values})
	}
	return out, nil
}

// Stream is one Loki push stream: labels plus (timestamp_ns, line, metadata) triples.
type Stream struct {
	Labels map[string]string
	Values []StreamValue
}

// StreamValue is one pushed line.
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
		return fmt.Errorf("loki push %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}
	return nil
}
