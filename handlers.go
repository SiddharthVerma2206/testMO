package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rootFS selects the one filesystem worth alerting on. Repeated rather than
// built up by concatenation so each expression below stays greppable as the
// PromQL it actually is.
const rootFS = `mountpoint="/",fstype!~"tmpfs|overlay|squashfs"`

// physicalNIC excludes the virtual interfaces that would otherwise be counted
// twice. On a box running its node in Docker the same packet crosses eth0,
// docker0 and a veth pair, and a bare device!="lo" sums all three — inflating
// throughput 2-3x for reasons that have nothing to do with the network.
const physicalNIC = `device!~"lo|veth.*|docker.*|br-.*|virbr.*|tap.*|tun.*"`

// systemMetrics are universal across every server, so they live here rather
// than in a chain YAML. Each expression must reduce to exactly one series —
// hence the sum()/max() wrappers, since a box can have several NICs and
// several devices mounted at /.
//
// Every name carries its unit as a suffix — _pct, _bytes, _bps, _days — which
// is the only thing telling the dashboard how to format the number. The API
// returns bare floats, so a name without a unit is a number nobody can render.
// See the suffix table in chains/_template.yaml; chain YAMLs follow the same
// rule. Renaming one here is an API change: update the chain configs that
// mirror it and the dashboard together.
var systemMetrics = map[string]string{
	"testMO_cpu_usage_pct":    `100 - (avg(rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)`,
	"testMO_memory_usage_pct": `100 * (1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)`,
	"testMO_disk_usage_pct":   `100 * (1 - max(node_filesystem_avail_bytes{` + rootFS + `}) / max(node_filesystem_size_bytes{` + rootFS + `}))`,

	// No unit suffix: a load average is dimensionless. So is anything ending
	// in a window (_1m, _1h) — those name the lookback, not the unit.
	"testMO_load_1m": `node_load1`,

	// Bits per second, which is how a NIC is rated and how every other server
	// tool reports throughput — node_exporter counts bytes, hence the *8.
	//
	// irate, not rate: irate uses only the last two samples, so at a 5s scrape
	// this is the throughput over the last 5 seconds — live, the way `iftop`
	// is live. The [1m] window is not an averaging window; it is just enough
	// lookback for irate to still find two samples if a scrape was missed.
	// The old rate(...[5m]) averaged over five minutes and diluted a 30-second
	// burst roughly tenfold, which hid exactly the spikes worth seeing.
	"testMO_network_rx_bps": `sum(irate(node_network_receive_bytes_total{` + physicalNIC + `}[1m])) * 8`,
	"testMO_network_tx_bps": `sum(irate(node_network_transmit_bytes_total{` + physicalNIC + `}[1m])) * 8`,
}

// maxHistoryPoints matches Prometheus' own ceiling on a range query, so an
// absurd start/end/step combination fails here with a clear message instead
// of as an opaque upstream error.
const maxHistoryPoints = 11000

// historyRanges are the lookback windows /metrics/history accepts as ?range=.
//
// An allowlist rather than a free parse of the string, for two reasons:
// time.ParseDuration has no "d" unit, so "3d" is a parse error rather than
// three days; and the set doubles as the menu the dashboard offers and the
// list an error message can name.
var historyRanges = map[string]time.Duration{
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"3d":  3 * 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

const defaultHistoryRange = "24h"

// historyTargetPoints is how finely a range is sliced when the caller doesn't
// name a step. Enough to draw a chart, and far short of shipping a week of
// 5-second samples that no chart can plot.
const historyTargetPoints = 400

// minHistoryStep matches scrape_interval in install/prometheus.yml.tmpl. A
// finer step only interpolates between samples that were never taken.
const minHistoryStep = 5 * time.Second

// maxHistoryRange mirrors RETENTION in install/setup.sh. Past it the TSDB
// holds nothing, and without this check the reply would be a silently
// truncated series rather than an error. Move the two together.
const maxHistoryRange = 30 * 24 * time.Hour

type Server struct {
	cfg    *AgentConfig
	chain  *ChainConfig
	prom   *Prometheus
	prober *Prober

	// chainMetrics is the chain config's metric_map, resolved once, since every
	// request consults it.
	chainMetrics map[string]string
	started      time.Time
}

func NewServer(cfg *AgentConfig, chain *ChainConfig) *Server {
	return &Server{
		cfg:          cfg,
		chain:        chain,
		prom:         NewPrometheus(cfg.PrometheusURL),
		prober:       NewProber(chain),
		chainMetrics: chain.Metrics(),
		started:      time.Now(),
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Public. /health is a liveness probe; /internal/metrics is the scrape
	// endpoint Prometheus reads, which cannot send an API key. Neither is
	// exposed publicly — the agent binds to loopback and Nginx proxies only
	// /api/v1/.
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("GET /internal/metrics", s.handleExposition)

	// Everything else. Registered on a sub-mux so one auth wrapper covers the
	// whole /api/v1/ prefix, including paths that don't exist (a bad path
	// still shouldn't reveal itself to an unauthenticated caller).
	authed := http.NewServeMux()
	authed.HandleFunc("GET /api/v1/info", s.handleInfo)
	authed.HandleFunc("GET /api/v1/metrics", s.handleAllMetrics)
	authed.HandleFunc("GET /api/v1/metrics/system", s.handleSystemMetrics)
	authed.HandleFunc("GET /api/v1/metrics/chain", s.handleChainMetrics)
	authed.HandleFunc("GET /api/v1/metrics/history", s.handleHistory)
	mux.Handle("/api/v1/", withAuth(s.cfg.APIKey, authed))

	return withCORS(s.cfg.AllowedOrigins, mux)
}

type healthResponse struct {
	Status              string `json:"status"`
	AgentVersion        string `json:"agent_version"`
	UptimeSeconds       int64  `json:"uptime_seconds"`
	PrometheusReachable bool   `json:"prometheus_reachable"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	reachable := s.prom.Reachable()
	status := "ok"
	if !reachable {
		status = "degraded"
	}
	writeJSON(w, http.StatusOK, healthResponse{
		Status:              status,
		AgentVersion:        Version,
		UptimeSeconds:       int64(time.Since(s.started).Seconds()),
		PrometheusReachable: reachable,
	})
}

type infoResponse struct {
	NodeID            string   `json:"node_id"`
	ChainType         string   `json:"chain_type"`
	ClientName        string   `json:"client_name"`
	ChainConfigLoaded bool     `json:"chain_config_loaded"`
	AvailableMetrics  []string `json:"available_metrics"`
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	resp := infoResponse{
		NodeID:            s.cfg.NodeID,
		ChainConfigLoaded: s.chain != nil,
		AvailableMetrics:  s.chain.AvailableMetrics(),
	}
	if s.chain != nil {
		resp.ChainType = s.chain.ChainType
		resp.ClientName = s.chain.ClientName
	}
	writeJSON(w, http.StatusOK, resp)
}

// The three metric handlers share a response envelope but differ in which
// keys they carry, so they build it as a map rather than fight struct tags.
// An empty (but non-nil) map marshals to {}, which is what a node with no
// chain config should report.

func (s *Server) handleAllMetrics(w http.ResponseWriter, r *http.Request) {
	var system, chain map[string]*float64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); system = s.metricValues(systemMetrics) }()
	go func() { defer wg.Done(); chain = s.metricValues(s.chainMetrics) }()
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now().Unix(),
		"system":    system,
		"chain":     chain,
	})
}

func (s *Server) handleSystemMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now().Unix(),
		"system":    s.metricValues(systemMetrics),
	})
}

func (s *Server) handleChainMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now().Unix(),
		"chain":     s.metricValues(s.chainMetrics),
	})
}

// metricValues resolves a name->PromQL map concurrently.
//
// Every requested key appears in the result. A metric that has no data or
// whose query failed maps to nil, which marshals as JSON null — the key's
// presence tells the dashboard the metric exists for this chain, the null
// tells it the value is currently unreadable. Neither is reported as 0.
func (s *Server) metricValues(queries map[string]string) map[string]*float64 {
	out := make(map[string]*float64, len(queries))

	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, promql := range queries {
		wg.Add(1)
		go func() {
			defer wg.Done()

			var value *float64
			v, ok, err := s.prom.Query(promql)
			switch {
			case err != nil:
				log.Printf("query %s: %v", name, err)
			case ok:
				value = &v
			}

			mu.Lock()
			out[name] = value
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

type historyResponse struct {
	Metric string      `json:"metric"`
	Start  int64       `json:"start"`
	End    int64       `json:"end"`
	Step   int64       `json:"step"`
	Values []DataPoint `json:"values"`
}

// handleHistory answers in one of two shapes. With ?metric= it returns that
// single series. Without, it returns every system and chain series at once,
// under the same system/chain envelope /api/v1/metrics uses — so a dashboard
// fills all its charts in one call instead of one request per metric.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	start, end, step, err := historyWindow(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if name := q.Get("metric"); name != "" {
		promql, known := s.lookup(name)
		if !known {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown metric %q", name))
			return
		}
		values, err := s.prom.QueryRange(promql, start, end, step)
		if err != nil {
			log.Printf("history %s: %v", name, err)
			writeError(w, http.StatusBadGateway, "prometheus query failed")
			return
		}
		writeJSON(w, http.StatusOK, historyResponse{
			Metric: name,
			Start:  start.Unix(),
			End:    end.Unix(),
			Step:   int64(step.Seconds()),
			Values: values,
		})
		return
	}

	var system, chain map[string][]DataPoint
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); system = s.metricSeries(systemMetrics, start, end, step) }()
	go func() { defer wg.Done(); chain = s.metricSeries(s.chainMetrics, start, end, step) }()
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"start":  start.Unix(),
		"end":    end.Unix(),
		"step":   int64(step.Seconds()),
		"system": system,
		"chain":  chain,
	})
}

// historyWindow resolves the span and resolution from the query string.
//
// ?range= is the shortcut: one of the presets, ending now. ?start=/?end= are
// the escape hatch for a custom span and override the preset where they
// overlap. ?step= is derived from the resulting span unless the caller names
// one, so a 24h and a 7d request both come back at a size worth charting
// instead of the 7d one being seven times heavier.
func historyWindow(q url.Values) (time.Time, time.Time, time.Duration, error) {
	var zero time.Time

	name := q.Get("range")
	if name == "" {
		name = defaultHistoryRange
	}
	window, ok := historyRanges[name]
	if !ok {
		return zero, zero, 0, fmt.Errorf("unknown range %q, want one of %s", name, historyRangeNames())
	}

	end := time.Now()
	// end alone shifts the window back while keeping its width, so
	// ?range=24h&end=<yesterday> reads as "the 24h ending yesterday".
	if v := q.Get("end"); v != "" {
		ts, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return zero, zero, 0, errors.New("end must be a unix timestamp")
		}
		end = time.Unix(ts, 0)
	}
	start := end.Add(-window)
	if v := q.Get("start"); v != "" {
		ts, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return zero, zero, 0, errors.New("start must be a unix timestamp")
		}
		start = time.Unix(ts, 0)
	}

	if !end.After(start) {
		return zero, zero, 0, errors.New("end must be after start")
	}
	span := end.Sub(start)
	if span > maxHistoryRange {
		return zero, zero, 0, fmt.Errorf(
			"range spans %s, but Prometheus only retains %s",
			span.Round(time.Hour), maxHistoryRange)
	}

	step := stepFor(span)
	if v := q.Get("step"); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs <= 0 {
			return zero, zero, 0, errors.New("step must be a positive number of seconds")
		}
		step = time.Duration(secs) * time.Second
	}
	if points := span / step; points > maxHistoryPoints {
		return zero, zero, 0, fmt.Errorf(
			"range would return %d points, maximum is %d — widen step or narrow the range",
			points, maxHistoryPoints)
	}
	return start, end, step, nil
}

// stepFor slices a span into roughly historyTargetPoints samples, rounded to
// whole seconds and never finer than the scrape interval.
func stepFor(span time.Duration) time.Duration {
	step := (span / historyTargetPoints).Round(time.Second)
	if step < minHistoryStep {
		return minHistoryStep
	}
	return step
}

// historyRangeNames lists the presets shortest first, for the error message a
// caller sees after guessing a window that doesn't exist.
func historyRangeNames() string {
	names := make([]string, 0, len(historyRanges))
	for n := range historyRanges {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return historyRanges[names[i]] < historyRanges[names[j]] })
	return strings.Join(names, ", ")
}

// metricSeries is metricValues over a time range: the same name->PromQL map,
// the same guarantee that every requested key appears in the result.
//
// Where metricValues reports an unreadable metric as null, a series reports it
// as an empty array — the distinction it needs to make is "no samples in this
// window", which is a real answer and charts as a gap, not as a missing key.
func (s *Server) metricSeries(queries map[string]string, start, end time.Time, step time.Duration) map[string][]DataPoint {
	out := make(map[string][]DataPoint, len(queries))

	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, promql := range queries {
		wg.Add(1)
		go func() {
			defer wg.Done()

			values, err := s.prom.QueryRange(promql, start, end, step)
			if err != nil {
				log.Printf("history %s: %v", name, err)
				values = []DataPoint{}
			}

			mu.Lock()
			out[name] = values
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// handleExposition publishes the prober's gauges for Prometheus to scrape.
// With no probes configured this is simply empty, which scrapes fine.
func (s *Server) handleExposition(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if s.prober != nil {
		s.prober.WriteExposition(w)
	}
}

// lookup resolves a testMO name to PromQL. System names win over chain names,
// so a chain config can't shadow a universal metric.
func (s *Server) lookup(name string) (string, bool) {
	if promql, ok := systemMetrics[name]; ok {
		return promql, true
	}
	promql, ok := s.chainMetrics[name]
	return promql, ok
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
