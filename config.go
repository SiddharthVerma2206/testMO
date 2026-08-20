package main

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AgentConfig is /etc/testmo/agent.yaml.
type AgentConfig struct {
	APIKey        string `yaml:"api_key"`
	Listen        string `yaml:"listen"`
	PrometheusURL string `yaml:"prometheus_url"`
	ChainConfig   string `yaml:"chain_config"`
	NodeID        string `yaml:"node_id"`

	// AllowedOrigins are the browser origins allowed to read a response, in
	// the "scheme://host[:port]" form a browser sends in its Origin header.
	// Empty or absent means "*" — see allowedOrigin in middleware.go.
	AllowedOrigins []string `yaml:"allowed_origins"`
}

// ChainConfig is /etc/testmo/chains/<chain>.yaml.
//
// Everything is optional. A client with no native Prometheus endpoint omits
// metrics_port/metrics_path; one that needs no direct RPC polling omits rpc
// and local_calls.
type ChainConfig struct {
	ChainType  string `yaml:"chain_type"`
	ClientName string `yaml:"client_name"`

	// Where Prometheus scrapes the client's own metrics. Recorded here so
	// /api/v1/info can report it; the scrape itself is configured by setup.sh.
	MetricsPort int    `yaml:"metrics_port"`
	MetricsPath string `yaml:"metrics_path"`

	RPC        RPCConfig   `yaml:"rpc"`
	LocalCalls []LocalCall `yaml:"local_calls"`

	// testMO standard name -> PromQL. The only place testMO_* names are
	// defined, whether the value originates from the client's own exporter
	// or from a probe_* gauge this agent publishes.
	MetricMap       map[string]string `yaml:"metric_map"`
	OptionalMetrics map[string]string `yaml:"optional_metrics"`
}

// RPCConfig describes how to reach the node's RPC for direct probing.
type RPCConfig struct {
	URL       string     `yaml:"url"`
	Timeout   Duration   `yaml:"timeout"`
	Interval  Duration   `yaml:"interval"`
	BasicAuth *BasicAuth `yaml:"basic_auth"`
}

type BasicAuth struct {
	User     string `yaml:"user"`
	Password string `yaml:"password"`
}

// LocalCall is one RPC probe. Its Name becomes a Prometheus gauge.
type LocalCall struct {
	Name string `yaml:"name"`
	// Type is "jsonrpc" (default) or "http_get".
	Type string `yaml:"type"`
	// Method is the JSON-RPC method name, or the URL path for http_get.
	Method  string        `yaml:"method"`
	Params  []interface{} `yaml:"params"`
	Extract Extract       `yaml:"extract"`

	// Optional marks a call that is *expected* to fail in some node states,
	// and silences its failure log.
	//
	// eth_syncing is the case this exists for: it returns an object with
	// highestBlock while catching up and the bare value false once synced, so
	// a probe reading highestBlock fails on every poll of a healthy node. At
	// a 5s interval that is 17,000 log lines a day saying nothing, and real
	// probe failures drown in them.
	//
	// Behaviour is otherwise unchanged: the gauge still drops on failure and
	// probe_call_up still goes to 0, so absence remains visible in PromQL.
	// Only the log line is suppressed.
	Optional bool `yaml:"optional"`
}

// Extract says how to turn a JSON response into a single float.
//
// Path is a dot-path with numeric segments for arrays ("sync_info.height",
// "0.slot"). For jsonrpc calls it is relative to the response's "result"
// field; for http_get it is relative to the whole body. An empty Path uses
// the value as-is.
type Extract struct {
	Path    string             `yaml:"path"`
	Type    string             `yaml:"type"`
	Values  map[string]float64 `yaml:"values"`  // type: match
	Default *float64           `yaml:"default"` // type: match
}

// Duration lets YAML carry Go duration strings such as "15s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Or returns the configured duration, or def when unset.
func (d Duration) Or(def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return time.Duration(d)
}

// LoadAgentConfig reads agent.yaml, applying defaults for anything omitted.
func LoadAgentConfig(path string) (*AgentConfig, error) {
	c := &AgentConfig{
		Listen:        "127.0.0.1:3001",
		PrometheusURL: "http://127.0.0.1:9090",
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.APIKey == "" {
		return nil, fmt.Errorf("%s: api_key is required", path)
	}
	c.AllowedOrigins = normalizeOrigins(c.AllowedOrigins)
	return c, nil
}

// normalizeOrigins trims each entry to the form a browser actually sends —
// no surrounding space, no trailing slash — and drops blanks.
//
// It rejects nothing. A malformed origin simply never matches, which is
// already harmless, and refusing to start over a typo in this field would
// take the whole agent down for something only a browser cares about. The
// tidying exists because "https://dashboard.argusnodes.com/" fails silently:
// the only symptom is a CORS error in someone else's console.
func normalizeOrigins(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, o := range in {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			out = append(out, o)
		}
	}
	return out
}

// LoadChainConfig reads a chain YAML. A missing path, a missing file or an
// empty one all yield (nil, nil): the agent still serves system metrics, it
// just reports no chain metrics.
func LoadChainConfig(path string) (*ChainConfig, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}

	c := &ChainConfig{}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// validate catches config mistakes at startup rather than at query time.
func (c *ChainConfig) validate() error {
	seen := make(map[string]bool, len(c.LocalCalls))
	for i, lc := range c.LocalCalls {
		switch {
		case lc.Name == "":
			return fmt.Errorf("local_calls[%d]: name is required", i)
		case seen[lc.Name]:
			return fmt.Errorf("local_calls[%d]: duplicate name %q", i, lc.Name)
		case lc.Name == metricCallDuration || lc.Name == metricCallUp:
			return fmt.Errorf("local_calls[%d]: %q is reserved", i, lc.Name)
		case lc.Method == "":
			return fmt.Errorf("local_calls[%d] (%s): method is required", i, lc.Name)
		}
		seen[lc.Name] = true

		switch lc.Type {
		case "", CallJSONRPC, CallHTTPGet:
		default:
			return fmt.Errorf("local_calls[%d] (%s): unknown type %q, want %q or %q",
				i, lc.Name, lc.Type, CallJSONRPC, CallHTTPGet)
		}

		if !extractTypes[lc.Extract.Type] {
			return fmt.Errorf("local_calls[%d] (%s): unknown extract type %q",
				i, lc.Name, lc.Extract.Type)
		}
		if lc.Extract.Type == ExtractMatch && len(lc.Extract.Values) == 0 && lc.Extract.Default == nil {
			return fmt.Errorf("local_calls[%d] (%s): extract type %q needs values or a default",
				i, lc.Name, ExtractMatch)
		}
	}

	if len(c.LocalCalls) > 0 && c.RPC.URL == "" {
		return fmt.Errorf("rpc.url is required when local_calls are defined")
	}

	for name, promql := range c.MetricMap {
		if promql == "" {
			return fmt.Errorf("metric_map[%s]: empty PromQL", name)
		}
	}
	for name, promql := range c.OptionalMetrics {
		if promql == "" {
			return fmt.Errorf("optional_metrics[%s]: empty PromQL", name)
		}
		if _, dup := c.MetricMap[name]; dup {
			return fmt.Errorf("optional_metrics[%s]: already defined in metric_map", name)
		}
	}
	return nil
}

// Metrics returns every testMO name the chain exposes, required and optional
// merged. Safe on a nil config.
func (c *ChainConfig) Metrics() map[string]string {
	if c == nil {
		return nil
	}
	out := make(map[string]string, len(c.MetricMap)+len(c.OptionalMetrics))
	for k, v := range c.MetricMap {
		out[k] = v
	}
	for k, v := range c.OptionalMetrics {
		out[k] = v
	}
	return out
}

// AvailableMetrics lists those names in sorted order, for /api/v1/info.
func (c *ChainConfig) AvailableMetrics() []string {
	m := c.Metrics()
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
