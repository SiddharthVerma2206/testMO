package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Transport kinds for a local_call.
const (
	CallJSONRPC = "jsonrpc"
	CallHTTPGet = "http_get"
)

// Extraction kinds for a local_call.
const (
	ExtractNumber = "number"
	ExtractHex    = "hex"
	ExtractBool   = "bool"
	ExtractCount  = "count"
	ExtractMatch  = "match"
)

var extractTypes = map[string]bool{
	ExtractNumber: true,
	ExtractHex:    true,
	ExtractBool:   true,
	ExtractCount:  true,
	ExtractMatch:  true,
}

// Gauges published for every call automatically.
const (
	metricCallDuration = "probe_call_duration_ms"
	metricCallUp       = "probe_call_up"
)

const maxRPCResponse = 4 << 20

// Prober polls the node's RPC on a fixed interval and holds each call's
// latest value in memory. Prometheus scrapes those back out via
// /internal/metrics, which is what gives probed data history — the agent
// never queries the node on the API request path.
type Prober struct {
	url      string
	auth     *BasicAuth
	calls    []LocalCall
	interval time.Duration
	http     *http.Client

	mu     sync.RWMutex
	values map[gaugeKey]float64
}

// gaugeKey identifies one exported series. Call is the local_call name, empty
// for the per-call value gauges themselves.
type gaugeKey struct {
	name string
	call string
}

// NewProber returns nil when the chain config defines no calls, which is a
// valid configuration — the caller simply skips probing.
func NewProber(c *ChainConfig) *Prober {
	if c == nil || len(c.LocalCalls) == 0 {
		return nil
	}
	return &Prober{
		url:      strings.TrimRight(c.RPC.URL, "/"),
		auth:     c.RPC.BasicAuth,
		calls:    c.LocalCalls,
		interval: c.RPC.Interval.Or(15 * time.Second),
		http:     &http.Client{Timeout: c.RPC.Timeout.Or(5 * time.Second)},
		values:   map[gaugeKey]float64{},
	}
}

// Run polls until ctx is cancelled. Call it in a goroutine.
func (p *Prober) Run(ctx context.Context) {
	p.pollAll(ctx)

	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.pollAll(ctx)
		}
	}
}

// pollAll probes every call concurrently and swaps in a whole new snapshot.
// Building a fresh map is what makes a newly-failing call disappear from the
// exposition instead of reporting a stale value forever.
func (p *Prober) pollAll(ctx context.Context) {
	next := make(map[gaugeKey]float64, len(p.calls)*3)

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range p.calls {
		wg.Add(1)
		go func() {
			defer wg.Done()

			start := time.Now()
			v, err := p.probe(ctx, c)
			ms := float64(time.Since(start).Microseconds()) / 1000

			mu.Lock()
			defer mu.Unlock()
			next[gaugeKey{metricCallDuration, c.Name}] = ms
			if err != nil {
				next[gaugeKey{metricCallUp, c.Name}] = 0
				// An optional call failing is a normal node state, not an
				// incident — see LocalCall.Optional. The gauge still drops
				// and probe_call_up still reports 0; only the log is quiet.
				if !c.Optional {
					log.Printf("probe %s (%s): %v", c.Name, c.Method, err)
				}
				return
			}
			next[gaugeKey{metricCallUp, c.Name}] = 1
			next[gaugeKey{name: c.Name}] = v
		}()
	}
	wg.Wait()

	p.mu.Lock()
	p.values = next
	p.mu.Unlock()
}

func (p *Prober) probe(ctx context.Context, c LocalCall) (float64, error) {
	body, err := p.fetch(ctx, c)
	if err != nil {
		return 0, err
	}
	node, err := walk(body, c.Extract.Path)
	if err != nil {
		return 0, err
	}
	return extractValue(node, c.Extract)
}

// fetch performs one call and returns the JSON value to extract from: the
// "result" field for jsonrpc, the whole body for http_get.
func (p *Prober) fetch(ctx context.Context, c LocalCall) (interface{}, error) {
	if c.Type == CallHTTPGet {
		return p.fetchGet(ctx, c)
	}
	return p.fetchJSONRPC(ctx, c)
}

func (p *Prober) fetchGet(ctx context.Context, c LocalCall) (interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url+c.Method, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var v interface{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRPCResponse)).Decode(&v); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return v, nil
}

func (p *Prober) fetchJSONRPC(ctx context.Context, c LocalCall) (interface{}, error) {
	params := c.Params
	if params == nil {
		params = []interface{}{} // some clients reject a null params field
	}
	payload, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  c.Method,
		Params:  params,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// bitcoind returns RPC errors with a 500 and a useful JSON body, so parse
	// first and only fall back to the status code if there's nothing better.
	var rr rpcResponse
	decErr := json.NewDecoder(io.LimitReader(resp.Body, maxRPCResponse)).Decode(&rr)
	switch {
	case rr.Error != nil:
		return nil, fmt.Errorf("rpc error %d: %s", rr.Error.Code, rr.Error.Message)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	case decErr != nil:
		return nil, fmt.Errorf("decode response: %w", decErr)
	case len(rr.Result) == 0:
		return nil, nil // no result field at all
	}

	var v interface{}
	if err := json.Unmarshal(rr.Result, &v); err != nil {
		return nil, fmt.Errorf("decode result: %w", err)
	}
	return v, nil
}

func (p *Prober) do(req *http.Request) (*http.Response, error) {
	if p.auth != nil {
		req.SetBasicAuth(p.auth.User, p.auth.Password)
	}
	return p.http.Do(req)
}

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// walk follows a dot-path such as "result.sync_info.catching_up" or "0.slot".
// An empty path returns v unchanged.
func walk(v interface{}, path string) (interface{}, error) {
	if path == "" {
		return v, nil
	}
	for _, seg := range strings.Split(path, ".") {
		switch cur := v.(type) {
		case map[string]interface{}:
			child, ok := cur[seg]
			if !ok {
				return nil, fmt.Errorf("path %q: no key %q", path, seg)
			}
			v = child
		case []interface{}:
			i, err := strconv.Atoi(seg)
			if err != nil {
				return nil, fmt.Errorf("path %q: %q is not an array index", path, seg)
			}
			if i < 0 || i >= len(cur) {
				return nil, fmt.Errorf("path %q: index %d out of range (len %d)", path, i, len(cur))
			}
			v = cur[i]
		default:
			return nil, fmt.Errorf("path %q: cannot descend into %T at %q", path, v, seg)
		}
	}
	return v, nil
}

// extractValue turns one JSON value into a float per the configured rule.
func extractValue(v interface{}, e Extract) (float64, error) {
	switch e.Type {
	case ExtractNumber:
		switch n := v.(type) {
		case float64:
			return n, nil
		case string: // Tendermint and others quote their integers
			f, err := strconv.ParseFloat(n, 64)
			if err != nil {
				return 0, fmt.Errorf("number: %q is not numeric", n)
			}
			return f, nil
		}
		return 0, fmt.Errorf("number: got %T", v)

	case ExtractHex:
		s, ok := v.(string)
		if !ok {
			return 0, fmt.Errorf("hex: got %T, want string", v)
		}
		n, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
		if err != nil {
			return 0, fmt.Errorf("hex: %q: %w", s, err)
		}
		return float64(n), nil

	case ExtractBool:
		if v == nil || v == false {
			return 0, nil
		}
		return 1, nil

	case ExtractCount:
		arr, ok := v.([]interface{})
		if !ok {
			return 0, fmt.Errorf("count: got %T, want array", v)
		}
		return float64(len(arr)), nil

	case ExtractMatch:
		s, ok := v.(string)
		if !ok {
			s = fmt.Sprint(v)
		}
		if n, found := e.Values[s]; found {
			return n, nil
		}
		if e.Default != nil {
			return *e.Default, nil
		}
		return 0, fmt.Errorf("match: %q not in values and no default set", s)
	}
	return 0, fmt.Errorf("unknown extract type %q", e.Type)
}

// WriteExposition renders the current gauges in Prometheus text format.
func (p *Prober) WriteExposition(w io.Writer) {
	p.mu.RLock()
	snapshot := p.values // replaced wholesale by pollAll, never mutated in place
	p.mu.RUnlock()

	keys := make([]gaugeKey, 0, len(snapshot))
	for k := range snapshot {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].name != keys[j].name {
			return keys[i].name < keys[j].name
		}
		return keys[i].call < keys[j].call
	})

	prev := ""
	for _, k := range keys {
		if k.name != prev {
			fmt.Fprintf(w, "# TYPE %s gauge\n", k.name)
			prev = k.name
		}
		value := strconv.FormatFloat(snapshot[k], 'g', -1, 64)
		if k.call == "" {
			fmt.Fprintf(w, "%s %s\n", k.name, value)
		} else {
			fmt.Fprintf(w, "%s{call=%q} %s\n", k.name, k.call, value)
		}
	}
}
