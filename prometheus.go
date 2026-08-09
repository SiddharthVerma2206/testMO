package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxPromResponse caps how much of a Prometheus reply we will read, so a
// pathological range query can't exhaust memory.
const maxPromResponse = 8 << 20

// Prometheus is a minimal client for the Prometheus HTTP API.
type Prometheus struct {
	base string
	http *http.Client
}

func NewPrometheus(baseURL string) *Prometheus {
	return &Prometheus{
		base: strings.TrimRight(baseURL, "/"),
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// DataPoint is one sample of a range query.
type DataPoint struct {
	Timestamp int64   `json:"t"`
	Value     float64 `json:"v"`
}

// Query runs an instant query and returns the first series' value.
//
// The bool reports whether a usable sample came back. An empty result vector
// (metric absent, or not scraped yet) and a NaN/Inf value both return false
// rather than a misleading 0 — callers surface that as JSON null.
func (p *Prometheus) Query(promql string) (float64, bool, error) {
	var out apiResponse
	if err := p.get("/api/v1/query", url.Values{"query": {promql}}, &out); err != nil {
		return 0, false, err
	}
	if len(out.Data.Result) == 0 {
		return 0, false, nil
	}
	v := out.Data.Result[0].Value.V
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false, nil
	}
	return v, true, nil
}

// QueryRange runs a range query and returns the first series' samples.
// NaN/Inf points are dropped; an absent metric yields an empty slice.
func (p *Prometheus) QueryRange(promql string, start, end time.Time, step time.Duration) ([]DataPoint, error) {
	q := url.Values{
		"query": {promql},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}
	var out apiResponse
	if err := p.get("/api/v1/query_range", q, &out); err != nil {
		return nil, err
	}
	if len(out.Data.Result) == 0 {
		return []DataPoint{}, nil
	}

	src := out.Data.Result[0].Values
	points := make([]DataPoint, 0, len(src))
	for _, s := range src {
		if math.IsNaN(s.V) || math.IsInf(s.V, 0) {
			continue
		}
		points = append(points, DataPoint{Timestamp: int64(s.T), Value: s.V})
	}
	return points, nil
}

// Reachable reports whether Prometheus is answering queries. vector(1) needs
// no stored data, so this tests the server rather than the scrape state.
func (p *Prometheus) Reachable() bool {
	_, _, err := p.Query("vector(1)")
	return err == nil
}

// apiResponse covers both instant ("vector") and range ("matrix") replies.
type apiResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value  sample   `json:"value"`  // vector
			Values []sample `json:"values"` // matrix
		} `json:"result"`
	} `json:"data"`
}

// sample decodes Prometheus' [<unix seconds>, "<value>"] pair, where the
// value is always a string so that NaN and Inf survive JSON.
type sample struct {
	T float64
	V float64
}

func (s *sample) UnmarshalJSON(b []byte) error {
	var raw []interface{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) != 2 {
		return fmt.Errorf("sample: want [timestamp, value], got %d elements", len(raw))
	}
	ts, ok := raw[0].(float64)
	if !ok {
		return fmt.Errorf("sample: timestamp is %T, want number", raw[0])
	}
	str, ok := raw[1].(string)
	if !ok {
		return fmt.Errorf("sample: value is %T, want string", raw[1])
	}
	v, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return fmt.Errorf("sample: %w", err)
	}
	s.T, s.V = ts, v
	return nil
}

func (p *Prometheus) get(path string, q url.Values, out *apiResponse) error {
	resp, err := p.http.Get(p.base + path + "?" + q.Encode())
	if err != nil {
		return fmt.Errorf("prometheus unreachable: %w", err)
	}
	defer resp.Body.Close()

	// Query errors arrive as 400/422 with a JSON body naming the problem, so
	// decode before judging the status code.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPromResponse)).Decode(out); err != nil {
		return fmt.Errorf("prometheus %s: unreadable response (HTTP %d): %w", path, resp.StatusCode, err)
	}
	if out.Status != "success" {
		if out.Error != "" {
			return fmt.Errorf("prometheus %s: %s", path, out.Error)
		}
		return fmt.Errorf("prometheus %s: status %q (HTTP %d)", path, out.Status, resp.StatusCode)
	}
	return nil
}
