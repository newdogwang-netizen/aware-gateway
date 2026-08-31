package pool

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/aware/gateway/internal/config"
)

// Endpoint represents a single upstream backend.
type Endpoint struct {
	Name          string
	URL           *url.URL
	Weight        int
	Timeout       time.Duration
	TLSSkipVerify bool
	HealthPath    string
	AuthToken     string
	Models        []string // models declared in static config

	// discoveredModels holds models discovered via /v1/models API.
	// Updated atomically by the discovery goroutine.
	discoveredModels atomic.Value // []string

	healthy  atomic.Bool
	inflight atomic.Int64
	breaker  *gobreaker.CircuitBreaker[struct{}]

	mu         sync.Mutex
	currentW   int
	effectiveW int
}

func (e *Endpoint) Healthy() bool     { return e.healthy.Load() }
func (e *Endpoint) InFlight() int64   { return e.inflight.Load() }
func (e *Endpoint) SetHealthy(v bool) { e.healthy.Store(v) }
func (e *Endpoint) Inc()              { e.inflight.Add(1) }
func (e *Endpoint) Dec()              { e.inflight.Add(-1) }

func (e *Endpoint) BreakerState() string {
	if e.breaker == nil {
		return "closed"
	}
	return e.breaker.State().String()
}

func (e *Endpoint) BreakerOpen() bool {
	if e.breaker == nil {
		return false
	}
	return e.breaker.State() == gobreaker.StateOpen
}

// AllModels returns the union of statically configured and dynamically
// discovered models for this endpoint.
func (e *Endpoint) AllModels() []string {
	static := e.Models
	discovered, _ := e.discoveredModels.Load().([]string)
	seen := make(map[string]bool, len(static)+len(discovered))
	var out []string
	for _, m := range static {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	for _, m := range discovered {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// ServesModel returns true if this endpoint serves the given model.
// Checks both statically configured and dynamically discovered models.
// If the endpoint has no model info at all (empty static + empty discovered),
// returns true — we assume it can serve anything (backward compatibility).
func (e *Endpoint) ServesModel(model string) bool {
	if model == "" {
		return true
	}
	for _, m := range e.Models {
		if m == model {
			return true
		}
	}
	discovered, _ := e.discoveredModels.Load().([]string)
	for _, m := range discovered {
		if m == model {
			return true
		}
	}
	// No model info at all → assume it serves everything
	if len(e.Models) == 0 && len(discovered) == 0 {
		return true
	}
	return false
}

// SetDiscoveredModels atomically updates the discovered model list.
func (e *Endpoint) SetDiscoveredModels(models []string) {
	sort.Strings(models)
	e.discoveredModels.Store(models)
}

func (e *Endpoint) httpClient() *http.Client {
	return &http.Client{
		Transport: e.transport(),
	}
}

// transport returns an http.RoundTripper with endpoint-specific TLS config.
func (e *Endpoint) transport() http.RoundTripper {
	if e.URL.Scheme == "https" && e.TLSSkipVerify {
		return &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec
			},
		}
	}
	return http.DefaultTransport
}

func (e *Endpoint) ExecuteWithBreaker(fn func() (struct{}, error)) (struct{}, error) {
	if e.breaker == nil {
		return fn()
	}
	return e.breaker.Execute(fn)
}

// EndpointStatus is the JSON-serializable status of an endpoint.
type EndpointStatus struct {
	Name             string   `json:"name"`
	URL              string   `json:"url"`
	Healthy          bool     `json:"healthy"`
	InFlight         int64    `json:"in_flight"`
	BreakerState     string   `json:"breaker_state"`
	Weight           int      `json:"weight,omitempty"`
	Models           []string `json:"models,omitempty"`
	DiscoveredModels []string `json:"discovered_models,omitempty"`
}

// PoolStatus is the JSON-serializable status of a pool.
type PoolStatus struct {
	Strategy  string           `json:"strategy"`
	Endpoints []EndpointStatus `json:"endpoints"`
	Fallback  string           `json:"fallback,omitempty"`
	AllModels []string         `json:"all_models,omitempty"` // union of all endpoint models
}

// Pool is a group of endpoints with a load-balancing strategy.
type Pool struct {
	Name      string
	Strategy  string
	Endpoints []*Endpoint
	Fallback  string
	current   atomic.Uint64
	cfg       config.HealthCheckConfig
	cancel    context.CancelFunc

	// discovery control
	discoveryEnabled bool
}

func NewPool(name string, cfg config.PoolConfig, cbCfg config.CircuitBreakerConfig) (*Pool, error) {
	endpoints := make([]*Endpoint, 0, len(cfg.Endpoints))
	for _, ec := range cfg.Endpoints {
		u, err := url.Parse(ec.URL)
		if err != nil {
			return nil, err
		}
		ep := &Endpoint{
			Name:          ec.Name,
			URL:           u,
			Weight:        ec.Weight,
			Timeout:       ec.Timeout,
			TLSSkipVerify: ec.TLSSkipVerify,
			HealthPath:    ec.HealthPath,
			AuthToken:     ec.AuthToken,
			Models:        ec.Models,
		}
		ep.healthy.Store(true)
		ep.effectiveW = ec.Weight
		ep.currentW = 0

		if cbCfg.Threshold > 0 {
			ep.breaker = gobreaker.NewCircuitBreaker[struct{}](gobreaker.Settings{
				Name:        ec.Name,
				MaxRequests: 1,
				Interval:    cbCfg.Window,
				Timeout:     cbCfg.OpenDuration,
				ReadyToTrip: func(counts gobreaker.Counts) bool {
					return counts.ConsecutiveFailures >= uint32(cbCfg.Threshold)
				},
				OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
					slog.Info("circuit breaker state change",
						"endpoint", name,
						"from", from.String(),
						"to", to.String(),
					)
				},
			})
		}
		endpoints = append(endpoints, ep)
	}
	return &Pool{
		Name:      name,
		Strategy:  cfg.Strategy,
		Endpoints: endpoints,
		Fallback:  cfg.Fallback,
		current:   atomic.Uint64{},
		cfg:       cfg.HealthCheck,
	}, nil
}

func (p *Pool) GetStatus() PoolStatus {
	endpoints := make([]EndpointStatus, len(p.Endpoints))
	modelSet := make(map[string]bool)
	for i, ep := range p.Endpoints {
		es := EndpointStatus{
			Name:         ep.Name,
			URL:          ep.URL.String(),
			Healthy:      ep.Healthy(),
			InFlight:     ep.InFlight(),
			BreakerState: ep.BreakerState(),
		}
		if p.Strategy == "weighted" {
			es.Weight = ep.Weight
		}
		allModels := ep.AllModels()
		if len(allModels) > 0 {
			es.Models = allModels
			for _, m := range allModels {
				modelSet[m] = true
			}
		}
		discovered, _ := ep.discoveredModels.Load().([]string)
		if len(discovered) > 0 {
			es.DiscoveredModels = discovered
		}
		endpoints[i] = es
	}
	var allModels []string
	for m := range modelSet {
		allModels = append(allModels, m)
	}
	sort.Strings(allModels)
	return PoolStatus{
		Strategy:  p.Strategy,
		Endpoints: endpoints,
		Fallback:  p.Fallback,
		AllModels: allModels,
	}
}

// AllModels returns the union of all models served by any endpoint in this pool.
func (p *Pool) AllModels() []string {
	modelSet := make(map[string]bool)
	for _, ep := range p.Endpoints {
		for _, m := range ep.AllModels() {
			modelSet[m] = true
		}
	}
	var out []string
	for m := range modelSet {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// Next picks the next available endpoint based on strategy.
func (p *Pool) Next() *Endpoint {
	switch p.Strategy {
	case "round_robin":
		return p.nextRoundRobin()
	case "least_conn":
		return p.nextLeastConn()
	case "weighted":
		return p.nextWeighted()
	default:
		return p.nextRoundRobin()
	}
}

// NextForModel picks the next available endpoint that serves the given model.
// If model is empty, delegates to Next() (no model filter).
// If no endpoint explicitly serves the model, falls back to endpoints with
// no model info (assume they serve everything), then to Next() as last resort.
func (p *Pool) NextForModel(model string) *Endpoint {
	if model == "" {
		return p.Next()
	}

	// First pass: find healthy endpoints that explicitly serve this model
	var candidates []*Endpoint
	var noInfoEndpoints []*Endpoint
	for _, ep := range p.Endpoints {
		if !ep.Healthy() || ep.BreakerOpen() {
			continue
		}
		if ep.ServesModel(model) {
			if len(ep.Models) > 0 || hasDiscovered(ep, model) {
				candidates = append(candidates, ep)
			} else {
				noInfoEndpoints = append(noInfoEndpoints, ep)
			}
		}
	}

	if len(candidates) > 0 {
		return p.pickFromCandidates(candidates)
	}
	if len(noInfoEndpoints) > 0 {
		return p.pickFromCandidates(noInfoEndpoints)
	}
	// Last resort: regular Next() — might not serve the model, but
	// better than 503. The upstream will return 404 if it doesn't know the model.
	return p.Next()
}

func hasDiscovered(ep *Endpoint, model string) bool {
	discovered, _ := ep.discoveredModels.Load().([]string)
	for _, m := range discovered {
		if m == model {
			return true
		}
	}
	return false
}

// pickFromCandidates selects one endpoint from a list using the pool's strategy.
func (p *Pool) pickFromCandidates(candidates []*Endpoint) *Endpoint {
	if len(candidates) == 1 {
		return candidates[0]
	}
	switch p.Strategy {
	case "least_conn":
		var best *Endpoint
		var minInflight int64 = 1<<63 - 1
		for _, ep := range candidates {
			ifl := ep.InFlight()
			if ifl < minInflight {
				minInflight = ifl
				best = ep
			}
		}
		return best
	case "weighted":
		totalW := 0
		for _, ep := range candidates {
			totalW += ep.Weight
		}
		var best *Endpoint
		bestW := 0
		for _, ep := range candidates {
			ep.mu.Lock()
			ep.currentW += ep.Weight
			if best == nil || ep.currentW > bestW {
				best = ep
				bestW = ep.currentW
			}
			ep.mu.Unlock()
		}
		if best != nil {
			best.mu.Lock()
			best.currentW -= totalW
			best.mu.Unlock()
		}
		return best
	default: // round_robin
		n := p.current.Add(1)
		return candidates[n%uint64(len(candidates))]
	}
}

func (p *Pool) nextRoundRobin() *Endpoint {
	n := p.current.Add(1)
	total := uint64(len(p.Endpoints))
	for i := uint64(0); i < total; i++ {
		ep := p.Endpoints[(n+i)%total]
		if ep.Healthy() && !ep.BreakerOpen() {
			return ep
		}
	}
	return p.Endpoints[n%total]
}

func (p *Pool) nextLeastConn() *Endpoint {
	var best *Endpoint
	var minInflight int64 = 1<<63 - 1
	for _, ep := range p.Endpoints {
		if !ep.Healthy() || ep.BreakerOpen() {
			continue
		}
		ifl := ep.InFlight()
		if ifl < minInflight {
			minInflight = ifl
			best = ep
		}
	}
	if best == nil {
		return p.Endpoints[0]
	}
	return best
}

func (p *Pool) nextWeighted() *Endpoint {
	totalW := 0
	healthyCount := 0
	for _, ep := range p.Endpoints {
		if ep.Healthy() && !ep.BreakerOpen() {
			totalW += ep.Weight
			healthyCount++
		}
	}
	if healthyCount == 0 {
		return p.Endpoints[0]
	}
	if healthyCount == 1 {
		for _, ep := range p.Endpoints {
			if ep.Healthy() && !ep.BreakerOpen() {
				return ep
			}
		}
	}

	var best *Endpoint
	bestW := 0
	for _, ep := range p.Endpoints {
		ep.mu.Lock()
		if ep.Healthy() && !ep.BreakerOpen() {
			ep.currentW += ep.Weight
			if best == nil || ep.currentW > bestW {
				best = ep
				bestW = ep.currentW
			}
		}
		ep.mu.Unlock()
	}
	if best != nil {
		best.mu.Lock()
		best.currentW -= totalW
		best.mu.Unlock()
	}
	return best
}

// SelectEndpoint picks a specific endpoint by name.
// Returns nil if not found or unhealthy.
func (p *Pool) SelectEndpoint(name string) *Endpoint {
	for _, ep := range p.Endpoints {
		if ep.Name == name {
			if ep.Healthy() && !ep.BreakerOpen() {
				return ep
			}
			return nil
		}
	}
	return nil
}

// StartHealthChecks launches periodic health check and model discovery goroutines.
func (p *Pool) StartHealthChecks(ctx context.Context) {
	interval := p.cfg.Interval
	if interval == 0 {
		interval = 10 * time.Second
	}
	timeout := p.cfg.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, p.cancel = context.WithCancel(ctx)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.checkAll(ctx, timeout)
			}
		}
	}()

	// Model discovery runs on a slower cadence (every 5 health check intervals)
	// and only for endpoints that have a /v1/models endpoint or a health path
	// that returns model info.
	go func() {
		discoveryInterval := interval * 5
		if discoveryInterval < 30*time.Second {
			discoveryInterval = 30 * time.Second
		}
		ticker := time.NewTicker(discoveryInterval)
		defer ticker.Stop()

		// Run once immediately on startup
		p.discoverModels(ctx, timeout)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.discoverModels(ctx, timeout)
			}
		}
	}()
}

func (p *Pool) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *Pool) checkAll(ctx context.Context, timeout time.Duration) {
	for _, ep := range p.Endpoints {
		checkURL := ep.URL.String() + ep.HealthPath
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, checkURL, nil)
		if err != nil {
			cancel()
			continue
		}
		client := ep.httpClient()
		resp, err := client.Do(req)
		cancel()

		was := ep.Healthy()
		if err != nil || resp.StatusCode >= 500 {
			if resp != nil && resp.Body != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			ep.SetHealthy(false)
			if was {
				slog.Warn("endpoint unhealthy", "pool", p.Name, "endpoint", ep.Name, "error", err)
			}
		} else {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			ep.SetHealthy(true)
			if !was {
				slog.Info("endpoint recovered", "pool", p.Name, "endpoint", ep.Name)
			}
		}
	}
}

// discoverModels queries each endpoint's /v1/models API to dynamically
// discover what models it serves. Results are stored in endpoint.discoveredModels.
// Endpoints that don't support /v1/models (404, 401, 403) are silently skipped.
// 401/403 means the endpoint exists but needs auth — we still mark it as
// having no discovered models (static config or fallback will handle it).
func (p *Pool) discoverModels(ctx context.Context, timeout time.Duration) {
	// Use a longer timeout for discovery — model lists can be large (OpenRouter: 655KB, 396 models)
	discoveryTimeout := timeout * 3
	if discoveryTimeout < 30*time.Second {
		discoveryTimeout = 30 * time.Second
	}

	for _, ep := range p.Endpoints {
		if !ep.Healthy() {
			continue
		}

		discovered, discoveryErr := p.discoverEndpointModels(ctx, ep, discoveryTimeout)
		if discoveryErr != nil {
			slog.Debug("model discovery failed",
				"pool", p.Name, "endpoint", ep.Name, "error", discoveryErr)
			continue
		}
		if len(discovered) > 0 {
			oldModels, _ := ep.discoveredModels.Load().([]string)
			if !sliceEqual(oldModels, discovered) {
				slog.Info("model discovery updated",
					"pool", p.Name,
					"endpoint", ep.Name,
					"models_count", len(discovered),
					"models", discovered,
				)
			}
			ep.SetDiscoveredModels(discovered)
		}
	}
}

// discoverEndpointModels queries a single endpoint's /v1/models API.
// Returns the list of model IDs and any error.
func (p *Pool) discoverEndpointModels(ctx context.Context, ep *Endpoint, timeout time.Duration) ([]string, error) {
	modelsURL := ep.URL.String() + "/v1/models"
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}

	if ep.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+ep.AuthToken)
	}

	client := &http.Client{
		Timeout:   timeout,
		Transport: ep.transport(),
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	// 404/405: endpoint doesn't implement /v1/models (e.g. Whisper)
	// 401/403: endpoint requires auth we don't have
	if resp.StatusCode == 404 || resp.StatusCode == 405 || resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, nil
	}
	if resp.StatusCode != 200 {
		return nil, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4MB max
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}

	var models []string
	for _, m := range result.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
