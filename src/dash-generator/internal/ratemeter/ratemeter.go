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
