package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// systemMetrics are universal across every server, so they live here rather
// than in a chain YAML. Each expression must reduce to exactly one series —
// hence the sum()/max() wrappers, since a box can have several NICs and
// several devices mounted at /.
var systemMetrics = map[string]string{
	"testMO_cpu_usage":    `100 - (avg(rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)`,
	"testMO_memory_usage": `100 * (1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)`,
	"testMO_memory_total": `node_memory_MemTotal_bytes`,
	"testMO_disk_usage":   `100 * (1 - max(node_filesystem_avail_bytes{mountpoint="/",fstype!~"tmpfs|overlay|squashfs"}) / max(node_filesystem_size_bytes{mountpoint="/",fstype!~"tmpfs|overlay|squashfs"}))`,
	"testMO_disk_total":   `max(node_filesystem_size_bytes{mountpoint="/",fstype!~"tmpfs|overlay|squashfs"})`,
	"testMO_uptime":       `time() - node_boot_time_seconds`,
	"testMO_network_rx":   `sum(rate(node_network_receive_bytes_total{device!="lo"}[5m]))`,
	"testMO_network_tx":   `sum(rate(node_network_transmit_bytes_total{device!="lo"}[5m]))`,
	"testMO_load_1m":      `node_load1`,
	"testMO_load_5m":      `node_load5`,
}

// maxHistoryPoints matches Prometheus' own ceiling on a range query, so an
// absurd start/end/step combination fails here with a clear message instead
// of as an opaque upstream error.
const maxHistoryPoints = 11000

type Server struct {
	cfg    *AgentConfig
	chain  *ChainConfig
	prom   *Prometheus
	prober *Prober

	// chainMetrics is metric_map and optional_metrics pre-merged, since every
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

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	name := q.Get("metric")
	if name == "" {
		writeError(w, http.StatusBadRequest, "metric parameter is required")
		return
	}
	promql, known := s.lookup(name)
	if !known {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown metric %q", name))
		return
	}

	end := time.Now()
	start := end.Add(-time.Hour)
	step := time.Minute

	if v := q.Get("start"); v != "" {
		ts, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "start must be a unix timestamp")
			return
		}
		start = time.Unix(ts, 0)
	}
	if v := q.Get("end"); v != "" {
		ts, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "end must be a unix timestamp")
			return
		}
		end = time.Unix(ts, 0)
	}
	if v := q.Get("step"); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs <= 0 {
			writeError(w, http.StatusBadRequest, "step must be a positive number of seconds")
			return
		}
		step = time.Duration(secs) * time.Second
	}

	if !end.After(start) {
		writeError(w, http.StatusBadRequest, "end must be after start")
		return
	}
	if points := end.Sub(start) / step; points > maxHistoryPoints {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"range would return %d points, maximum is %d — widen step or narrow the range",
			points, maxHistoryPoints))
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
